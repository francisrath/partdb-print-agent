// Package tspl drives TSC printers and the TSPL/TSPL2 clones (Xprinter, Munbyn, iDPRT, HPRT, Gprinter …): every
// page is one BITMAP sent with its own SIZE/GAP setup, status is the one-byte <ESC>!? answer. Commands follow TSC's
// public TSPL/TSPL2 Programming Manual.
package tspl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() {
	driver.Register("tspl", func(m *devicedb.Model, _ driver.Options) (driver.Driver, error) {
		return &tspl{model: m, poll: 250 * time.Millisecond, notSeen: 2 * time.Second}, nil
	})
}

// drainWait is the read timeout while dropping stale bytes on transports that can shorten it.
const drainWait = 200 * time.Millisecond

// <ESC>!? status bits. The manual (TSPL/TSPL2 Programming Manual rev. 2026/4/30, p.122) lists the byte's values as
// a table (01 head open, 02 paper jam, 03 jam and head open, 04 out of paper … 20 printing, 80 other error),
// which reads as these bits; 0x40 isn't listed.
const (
	headOpen  = 0x01
	paperJam  = 0x02
	paperOut  = 0x04
	ribbonOut = 0x08
	paused    = 0x10
	printing  = 0x20
	otherErr  = 0x80
)

type tspl struct {
	model         *devicedb.Model
	poll, notSeen time.Duration // completion polling; tests shorten them
}

// dpm is the head's dots per mm (8 at 203 dpi, 12 at 300 dpi).
func (d *tspl) dpm() float64 { return math.Round(float64(d.model.DPI) / 25.4) }

func mm(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func (d *tspl) Encode(w io.Writer, job *driver.Job) error {
	m := job.Media
	if m == nil {
		return errors.New("tspl: no media")
	}
	var cut string // only when asked: SET CUTTER is kept over power-off, and a cutter is an option on every model
	if d.model.QuirkBool("cutter") {
		switch job.Cut {
		case "per_label":
			cut = "SET CUTTER 1"
		case "last":
			cut = "SET CUTTER BATCH"
		case "none":
			cut = "SET CUTTER OFF"
		}
	}
	switch job.Cut {
	case "", "per_label", "last", "none":
	default:
		return fmt.Errorf("tspl: unknown cut mode %q", job.Cut)
	}
	var b bytes.Buffer
	for _, pg := range job.Pages {
		length, gap := m.LengthMM, mm(float64(d.model.Quirk("gap_mm", 3)))
		if m.Kind == driver.Continuous || length == 0 {
			length, gap = float64(pg.Bounds().Dy())/d.dpm(), "0"
		}
		fmt.Fprintf(&b, "SIZE %s mm,%s mm\r\nGAP %s mm,0 mm\r\nDIRECTION 0,0\r\nREFERENCE 0,0\r\n", mm(m.WidthMM), mm(math.Round(length*10)/10), gap)
		if cut != "" {
			b.WriteString(cut + "\r\n")
		}
		b.WriteString("CLS\r\n")
		appendBitmap(&b, pg, d.model.HeadPins)
		b.WriteString("PRINT 1,1\r\n")
	}
	_, err := w.Write(b.Bytes())
	return err
}

// appendBitmap writes img, cropped to dots (the head; 0 = no limit), as BITMAP 0,0 in OVERWRITE mode. TSPL's bit
// sense is inverted: a 1 bit is white, 0 prints. Rows are whole bytes, the padding white.
func appendBitmap(b *bytes.Buffer, img *image.Gray, dots int) {
	r := img.Bounds()
	dx := r.Dx()
	if dots > 0 {
		dx = min(dx, dots)
	}
	wb := (dx + 7) / 8
	fmt.Fprintf(b, "BITMAP 0,0,%d,%d,0,", wb, r.Dy())
	row := make([]byte, wb)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for i := range row {
			row[i] = 0xFF
		}
		for x := 0; x < dx; x++ {
			if img.GrayAt(r.Min.X+x, y).Y < 128 {
				row[x/8] &^= 0x80 >> (x % 8)
			}
		}
		b.Write(row)
	}
	b.WriteString("\r\n")
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, driver.ErrNoReadback)
}

// drain drops what the printer sent late (an answer that came after its read timed out), so it isn't taken for
// the next answer.
func drain(t driver.Transport) error {
	if s, ok := t.(driver.ReadTimeoutSetter); ok {
		s.SetReadTimeout(drainWait)
		defer s.SetReadTimeout(0)
	}
	p := make([]byte, 64)
	for range 64 {
		n, err := t.Read(p)
		if n == 0 {
			if err != nil && !isTimeout(err) {
				return err
			}
			return nil
		}
	}
	return nil
}

// query sends <ESC>!? and reads the one-byte answer; ok is false when none came.
func query(t driver.Transport) (b byte, ok bool, err error) {
	if err := drain(t); err != nil {
		return 0, false, err
	}
	if _, err := t.Write([]byte{0x1B, '!', '?'}); err != nil {
		return 0, false, err
	}
	p := make([]byte, 1)
	n, err := t.Read(p)
	if n == 1 {
		return p[0], true, nil
	}
	if err != nil && !isTimeout(err) {
		return 0, false, err
	}
	return 0, false, nil
}

// code maps a status byte; the first fault in the manual's order wins.
func code(s byte) (driver.Code, string) {
	for _, e := range []struct {
		bit    byte
		code   driver.Code
		detail string
	}{
		{headOpen, driver.CoverOpen, "print head open"},
		{paperJam, driver.PaperJam, "paper jam"},
		{paperOut, driver.NoMedia, "out of labels"},
		{ribbonOut, driver.RibbonOut, "ribbon out"},
		{paused, driver.Paused, "printer is paused: press the feed button"},
		{otherErr, driver.Unknown, "printer error"},
	} {
		if s&e.bit != 0 {
			return e.code, e.detail
		}
	}
	if s&^printing != 0 {
		return driver.Unknown, fmt.Sprintf("printer status %02x", s)
	}
	return driver.OK, ""
}

func (d *tspl) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, ok, err := query(t)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, driver.ErrNoReadback
	}
	st := &driver.Status{Model: d.model.ID, Idle: s&printing == 0, Via: t.Kind(), Raw: fmt.Sprintf("%02x", s)}
	st.Code, st.Detail = code(s)
	return st, nil
}

// Print refuses on a bad job.Status, sends the job, then polls <ESC>!? : confirmed once the printing bit was seen
// and has cleared. A printer that gave no status before the job gets no polling ("sent (unconfirmed)").
func (d *tspl) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	st := job.Status
	if st != nil && st.Code != driver.OK {
		return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
	}
	var b bytes.Buffer
	if err := d.Encode(&b, job); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil { // stopped while rendering: send nothing
		return nil, err
	}
	if _, err := t.Write(b.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: b.Len()}
	if st == nil {
		res.Message = "sent (unconfirmed: the printer doesn't report status)"
		return res, nil
	}
	// From here the label may print: never report a failure that a retry would print twice, unless the printer says so.
	sent := time.Now()
	seen := false
	for end := sent.Add(10*time.Second + 2*time.Second*time.Duration(len(job.Pages))); time.Now().Before(end); {
		select {
		case <-ctx.Done():
			res.Message = "sent (unconfirmed: stopped waiting)"
			return res, nil
		case <-time.After(d.poll):
		}
		s, ok, err := query(t)
		if err != nil {
			res.Message = "sent (unconfirmed: connection dropped after the label was sent, it may have printed: " + err.Error() + ")"
			return res, nil
		}
		if !ok {
			continue
		}
		if c, detail := code(s); c != driver.OK {
			return nil, &driver.Error{Code: c, Detail: detail}
		}
		switch {
		case s&printing != 0:
			seen = true
		case seen:
			res.Confirmed, res.Message = true, "printed"
			return res, nil
		case time.Since(sent) > d.notSeen:
			res.Message = "sent, probably printed (the printer finished before we could see it)"
			return res, nil
		}
	}
	res.Message = "sent (unconfirmed: the printer is still busy)"
	return res, nil
}
