// Package escpos drives ESC/POS receipt printers (Bluetooth Classic SPP, e.g. REGO RG-MLP80B) with
// GS v 0 raster bitmaps. Commands follow Epson's public ESC/POS reference.
package escpos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() {
	driver.Register("escpos", func(m *devicedb.Model, _ driver.Options) (driver.Driver, error) {
		return &escpos{model: m, confirm: time.Duration(m.Quirk("confirm_timeout_ms", 20000)) * time.Millisecond}, nil
	})
}

const nearEnd = "paper near end"

const maxBand = 256 // raster lines per GS v 0 (some printers choke on more)

// Status bits (Epson DLE EOT): n=1 printer, n=2 offline cause, n=4 paper sensor.
const (
	offline     = 0x08 // n=1
	coverOpen   = 0x04 // n=2
	paperStop   = 0x20 // n=2: paper end stopped printing
	errorFlag   = 0x40 // n=2
	paperEnd    = 0x60 // n=4
	paperNear   = 0x0C // n=4
	gsrPaperEnd = 0x0C // GS r 1 reply
	gsrNearEnd  = 0x03
)

type escpos struct {
	model   *devicedb.Model
	confirm time.Duration // how long to wait for the GS r 1 answer; tests shorten it
}

func (d *escpos) Encode(w io.Writer, job *driver.Job) error {
	feed := d.model.Quirk("feed_lines", 80)
	cut := d.model.QuirkBool("cutter")
	var b []byte
	for _, pg := range job.Pages {
		b = append(b, 0x1B, 0x40)
		b = appendRaster(b, pg, d.model.HeadPins)
		for n := feed; n > 0; n -= 255 {
			b = append(b, 0x1B, 0x64, byte(min(n, 255)))
		}
		if cut {
			b = append(b, 0x1D, 0x56, 0x01)
		}
	}
	_, err := w.Write(b)
	return err
}

// appendRaster adds img, cropped to dots (the head), as GS v 0 bands, 1 = black, MSB first, rows padded to whole bytes.
func appendRaster(b []byte, img *image.Gray, dots int) []byte {
	r := img.Bounds()
	dx := min(r.Dx(), dots)
	wb := (dx + 7) / 8
	for y0 := r.Min.Y; y0 < r.Max.Y; y0 += maxBand {
		n := min(maxBand, r.Max.Y-y0)
		b = append(b, 0x1D, 0x76, 0x30, 0x00, byte(wb), byte(wb>>8), byte(n), byte(n>>8))
		for y := y0; y < y0+n; y++ {
			row := len(b)
			b = append(b, make([]byte, wb)...)
			for x := 0; x < dx; x++ {
				if img.GrayAt(r.Min.X+x, y).Y < 128 {
					b[row+x/8] |= 0x80 >> (x % 8)
				}
			}
		}
	}
	return b
}

func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, driver.ErrNoReadback)
}

// drain drops what the printer sent late (an answer that came after its read timed out), reading
// until a read brings nothing; it would otherwise be taken for the next answer.
func drain(t driver.Transport) error {
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

// query sends DLE EOT n and reads the one-byte answer; ok is false when none came.
func query(t driver.Transport, n byte) (b byte, ok bool, err error) {
	if _, err := t.Write([]byte{0x10, 0x04, n}); err != nil {
		return 0, false, err
	}
	p := make([]byte, 1)
	got, err := t.Read(p)
	if got > 0 {
		return p[0], true, nil
	}
	if err != nil && !isTimeout(err) {
		return 0, false, err
	}
	return 0, false, nil
}

func (d *escpos) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	var raw [3]byte
	if err := drain(t); err != nil { // once: the answers then come back in order
		return nil, err
	}
	for i, n := range []byte{1, 2, 4} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, ok, err := query(t, n)
		if err != nil {
			return nil, err
		}
		if !ok { // without the cover and paper bits we can't vouch for the printer
			return nil, driver.ErrNoReadback
		}
		raw[i] = b
	}
	st := &driver.Status{Code: driver.OK, Model: d.model.ID, Idle: true, Via: t.Kind(), Raw: fmt.Sprintf("% x", raw[:])}
	switch {
	case raw[1]&coverOpen != 0:
		st.Code, st.Detail = driver.CoverOpen, "cover open"
	case raw[2]&paperEnd != 0 || raw[1]&paperStop != 0:
		st.Code, st.Detail = driver.NoMedia, "out of paper"
	case raw[1]&errorFlag != 0:
		st.Code, st.Detail = driver.Unknown, "printer error"
	case raw[0]&offline != 0:
		st.Code, st.Detail = driver.Unknown, "printer offline"
	case raw[2]&paperNear != 0:
		st.Detail = nearEnd
	}
	return st, nil
}

// Print refuses on a bad status, sends the job, then asks GS r 1 (answered in order, so after the
// printing) and waits for the answer. No answer: sent, unconfirmed.
func (d *escpos) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	st := job.Status
	if st == nil {
		var err error
		if st, err = d.Status(ctx, t); err != nil && !errors.Is(err, driver.ErrNoReadback) {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
	}
	if st != nil && st.Code != driver.OK {
		return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
	}
	var b bytes.Buffer
	if err := d.Encode(&b, job); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := t.Write(b.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	res := &driver.Result{Pages: len(job.Pages), Bytes: b.Len()}
	if job.Media != nil {
		res.Media = job.Media.ID
	}
	if st != nil && st.Detail != "" {
		res.Warnings = append(res.Warnings, st.Detail)
	}
	// From here the label may print: never report a failure that a retry would print twice.
	res.Message = "sent (unconfirmed: no answer to GS r)"
	if err := drain(t); err != nil {
		return nil, dropped(err)
	}
	if _, err := t.Write([]byte{0x1D, 0x72, 0x01}); err != nil {
		return nil, dropped(err)
	}
	p := make([]byte, 1)
	for end := time.Now().Add(d.confirm); time.Now().Before(end); {
		if err := ctx.Err(); err != nil {
			res.Message = "sent (unconfirmed: stopped waiting: " + err.Error() + ")"
			return res, nil
		}
		n, err := t.Read(p)
		if n > 0 {
			switch {
			case p[0]&gsrPaperEnd != 0:
				return nil, &driver.Error{Code: driver.NoMedia, Detail: "out of paper while printing"}
			case p[0]&gsrNearEnd != 0:
				res.Warnings = append(res.Warnings, nearEnd)
			}
			res.Confirmed, res.Message = true, "printed"
			break
		}
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			return res, nil // no read channel: don't spin
		case err != nil && !isTimeout(err):
			return nil, dropped(err)
		case err == nil:
			time.Sleep(time.Millisecond) // (0, nil): no answer, don't spin
		}
	}
	return res, nil
}

func dropped(err error) error {
	return &driver.Error{Code: driver.CommsError, Err: err,
		Detail: "connection dropped after the label was sent, it may have printed: " + err.Error()}
}
