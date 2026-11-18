package brother

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
)

// QL dialect, from Brother's "Raster Command Reference" (QL-800/810W/820NWB).

const (
	qlInvalidateN = 400
	qlHeadPins    = 720 // 90 bytes per raster line
	qlLineBytes   = qlHeadPins / 8
)

type ql struct {
	model         *devicedb.Model
	opts          driver.Options
	noCompression bool                                           // tests: uncompressed raster lines
	query         func(ctx context.Context) (*snmp.State, error) // SNMP status (nil error = answered)
	poll          time.Duration                                  // SNMP poll interval after sending
}

func newQL(m *devicedb.Model, o driver.Options) *ql {
	d := &ql{model: m, opts: o, poll: 250 * time.Millisecond}
	d.query = func(ctx context.Context) (*snmp.State, error) {
		if o.NoSNMP || o.Host == "" {
			return nil, errors.New("snmp disabled")
		}
		community := o.SNMPCommunity
		if community == "" {
			community = "public"
		}
		return snmp.Query(ctx, o.Host, community, 2*time.Second)
	}
	return d
}

var qlStatusRequest = append(make([]byte, qlInvalidateN), 0x1b, '@', 0x1b, 'i', 'S')

// Status reads the printer over SNMP (network QLs give no status on 9100), else asks over
// the transport with ESC i S.
func (d *ql) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if ss, err := d.query(ctx); err == nil {
		return d.statusFromSNMP(ss), nil
	}
	if _, err := t.Write(qlStatusRequest); err != nil {
		return nil, err
	}
	b, err := readReply(t)
	if err != nil {
		return nil, err
	}
	s, err := parseQLStatus(b)
	if s != nil {
		s.Via = t.Kind() // answered over the printer connection itself (usb, tcp)
	}
	return s, err
}

func (d *ql) statusFromSNMP(ss *snmp.State) *driver.Status {
	st := &driver.Status{Code: driver.OK, Detail: ss.Display, Model: strings.TrimPrefix(ss.Model, "Brother "),
		Via: "snmp", Idle: ss.Idle}
	if prob := ss.Problem(); prob != nil {
		st.Code, st.Detail = prob.Code, prob.Detail
	}
	if kind, w, l, ok := ss.LoadedMedia(); ok {
		if m, ok := qlMediaBySize(d.model, kind, w, l); ok {
			st.Media = &m
		} else {
			st.Media = &driver.Media{ID: "unknown", Kind: kind, WidthMM: float64(w), LengthMM: float64(l)}
		}
	}
	return st
}

// qlMediaBySize maps a reported roll to one the model takes; with no model known (status
// from an unknown QL), to any QL's.
func qlMediaBySize(model *devicedb.Model, kind driver.MediaKind, w, l int) (driver.Media, bool) {
	db := devicedb.Default()
	models := db.Models("brother")
	if model != nil {
		models = []*devicedb.Model{model}
	}
	for _, m := range models {
		if m.Dialect != "ql" {
			continue
		}
		for _, md := range db.MediaFor(m) {
			if md.Kind == kind && int(md.WidthMM) == w && int(md.LengthMM) == l {
				return md, true
			}
		}
	}
	return driver.Media{}, false
}

func (d *ql) validate(job *driver.Job) error {
	m := job.Media
	if len(job.Pages) == 0 {
		return errors.New("no pages")
	}
	switch job.Cut {
	case "", "per_label", "last", "none":
	default:
		return fmt.Errorf("unknown cut mode %q", job.Cut)
	}
	maxLines := d.model.Quirk("max_lines", 11811)
	for i, p := range job.Pages {
		w, h := p.Bounds().Dx(), p.Bounds().Dy()
		if w > m.PrintableWidthDots {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots wide, %s prints %d", i+1, w, m.ID, m.PrintableWidthDots)}
		}
		if m.Kind == driver.Continuous && h > maxLines {
			return &driver.Error{Code: driver.Unsupported,
				Detail: fmt.Sprintf("page %d is %d dots long; continuous tape allows at most %d (1 m)", i+1, h, maxLines)}
		}
		if h == 0 || (m.Kind == driver.DieCut && h > m.PrintableLengthDots) {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots long, %s allows %d", i+1, h, m.ID, m.PrintableLengthDots)}
		}
	}
	return nil
}

func (d *ql) Encode(w io.Writer, job *driver.Job) error {
	if err := d.validate(job); err != nil {
		return err
	}
	m := job.Media
	minLines := d.model.Quirk("min_lines", 150)
	raw := d.noCompression || d.model.QuirkBool("no_compression")

	var b bytes.Buffer
	b.Write(make([]byte, qlInvalidateN))
	b.Write([]byte{0x1b, '@'})         // initialise
	b.Write([]byte{0x1b, 'i', 'a', 1}) // switch to raster mode

	var various, expanded byte
	switch job.Cut {
	case "", "per_label":
		various, expanded = 0x40, 0x08 // auto cut, cut at end
	case "last":
		expanded = 0x08
	}
	// flags: recover | quality | kind, width, length valid (same as the known-good reference)
	kind, flags, margin := byte(0x0a), byte(0xce), uint16(d.model.Quirk("feed_margin", 35))
	if m.Kind == driver.DieCut {
		kind, margin = 0x0b, 0
	}

	for i, p := range job.Pages {
		lines := p.Bounds().Dy()
		padTop, padBottom := 0, 0
		if lines < minLines && m.Kind == driver.Continuous { // short continuous label: centre it in the minimum length
			padTop = (minLines - lines) / 2
			padBottom = minLines - lines - padTop
		}
		start := byte(1)
		if i == 0 {
			start = 0
		}
		b.Write([]byte{0x1b, 'i', 'z', flags, kind, byte(m.WidthMM), byte(m.LengthMM)})
		b.Write(binary.LittleEndian.AppendUint32(nil, uint32(padTop+lines+padBottom)))
		b.Write([]byte{start, 0})
		b.Write([]byte{0x1b, 'i', 'M', various})
		if various&0x40 != 0 {
			b.Write([]byte{0x1b, 'i', 'A', 1}) // cut every label
		}
		b.Write([]byte{0x1b, 'i', 'K', expanded})
		b.Write(binary.LittleEndian.AppendUint16([]byte{0x1b, 'i', 'd'}, margin))
		if raw {
			b.Write([]byte{'M', 0})
		} else {
			b.Write([]byte{'M', 2})
		}
		blank := make([]byte, qlLineBytes)
		for y := -padTop; y < lines+padBottom; y++ {
			line := blank
			if y >= 0 && y < lines {
				line = packLine(p, y, *m)
			}
			if !raw {
				line = packbits(line)
			}
			b.Write([]byte{'g', 0, byte(len(line))})
			b.Write(line)
		}
		if i == len(job.Pages)-1 {
			b.WriteByte(0x1a) // print with feed
		} else {
			b.WriteByte(0x0c) // print
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// packLine maps image row y onto the 720-pin head. Pins are numbered from the
// spec's "right" edge, so the image's rightmost column lands on pin OffsetPins.
func packLine(p *image.Gray, y int, m driver.Media) []byte {
	line := make([]byte, qlLineBytes)
	w := p.Bounds().Dx()
	off := m.OffsetPins + (m.PrintableWidthDots-w)/2 // centre narrower pages
	row := p.Pix[y*p.Stride : y*p.Stride+w]
	for x, v := range row {
		if v < 128 {
			pin := off + (w - 1 - x)
			line[pin/8] |= 0x80 >> (pin % 8)
		}
	}
	return line
}

// Print sends the job and waits for the printer to confirm: over SNMP when the pre-print
// status came from SNMP, over the transport when the printer answered ESC i S there.
func (d *ql) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	var b bytes.Buffer
	if err := d.Encode(&b, job); err != nil {
		return nil, err
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: b.Len(),
		Message: "sent (unconfirmed: printer gives no status)"}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := t.Write(b.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	timeout := 30*time.Second + time.Duration(len(job.Pages))*5*time.Second
	switch {
	case job.Status != nil && job.Status.Via == "snmp":
		done, err := d.waitSNMP(ctx, timeout)
		switch {
		case err != nil:
			return nil, err
		case done && job.Status.Idle:
			res.Message, res.Confirmed = "printed", true
		case done:
			// it was busy when we sent: the printing we saw may have been another job
			res.Message = "sent (unconfirmed: printer was busy with another job)"
		default:
			res.Message = "sent (unconfirmed: printer didn't report printing)"
		}
	case job.Status != nil && job.Status.Via != "":
		err := waitDone(ctx, t, len(job.Pages), timeout)
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			res.Message = "sent (unconfirmed: no completion status received)"
		case err != nil:
			return nil, err
		default:
			res.Message, res.Confirmed = "printed", true
		}
	}
	return res, nil
}

// waitSNMP polls until the printer has printed and is idle again (done), reports an
// error, or the timeout passes (not done). SNMP has no per-job id, so Print only treats
// "done" as confirmation when the printer was idle before the job was sent.
// Observed on a QL-810W: idle → printing → other ("BUSY", cutting) → idle; errors show
// as device down plus a Printer-MIB alert.
// ponytail: "idle for 1 s after activity" = done; a printer that pauses longer between
// pages of a big job would be reported done early.
func (d *ql) waitSNMP(ctx context.Context, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	active, idle := false, 0
	for first := true; time.Now().Before(deadline); first = false {
		wait := d.poll
		if first {
			wait = 0 // a short job can finish within one poll interval
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
		ss, err := d.query(ctx)
		if err != nil {
			continue
		}
		if prob := ss.Problem(); prob != nil {
			return false, prob
		}
		switch {
		case ss.Printing:
			active, idle = true, 0
		case ss.Idle && active:
			if idle++; idle >= 4 {
				return true, nil
			}
		}
	}
	return false, nil
}

// waitDone reads status replies until n pages report "printing completed" or an error arrives.
// Only call it after a status request succeeded on t; returns driver.ErrNoReadback on timeout.
func waitDone(ctx context.Context, t driver.Transport, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	r := &replyReader{t: t} // keeps a partial reply until the deadline
	for done := 0; done < n; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			if r.n > 0 {
				return r.incomplete(driver.ErrNoReadback)
			}
			return driver.ErrNoReadback // printer keeps talking but never reports completion
		}
		b, err := r.read()
		if errors.Is(err, driver.ErrNoReadback) && !errors.Is(err, io.EOF) {
			continue // read timed out: long label still printing (the deadline check handles a partial reply)
		}
		if err != nil {
			return err
		}
		s, err := parseQLStatus(b)
		if err != nil {
			return err
		}
		if s.Code != driver.OK {
			return &driver.Error{Code: s.Code, Detail: s.Detail}
		}
		if s.Done {
			done++
		}
	}
	return nil
}

func parseQLStatus(b []byte) (*driver.Status, error) {
	if len(b) != 32 || b[0] != 0x80 || b[1] != 0x20 || b[2] != 'B' {
		return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("bad status reply % x", b)}
	}
	s := &driver.Status{Code: driver.OK, Model: fmt.Sprintf("unknown (0x%02x)", b[4])}
	model, ok := devicedb.Default().ModelByStatusCode("brother", int(b[4]))
	if ok {
		s.Model = model.ID
	}
	e1, e2 := b[8], b[9]
	for _, e := range []struct {
		hit    bool
		code   driver.Code
		detail string
	}{
		{e1&0x01 != 0, driver.NoMedia, "no media"},
		{e1&0x02 != 0, driver.NoMedia, "end of media"},
		{e1&0x04 != 0, driver.CutterJam, "cutter jam"},
		{e1&0x10 != 0, driver.Busy, "printer in use"},
		{e2&0x01 != 0, driver.MediaMismatch, "replace media"},
		{e2&0x10 != 0, driver.CoverOpen, "cover open"},
		{e2&0x20 != 0, driver.Overheat, "overheating"},
		{e2&0x40 != 0, driver.PaperJam, "media cannot be fed"},
		{e2&0x0e != 0, driver.CommsError, "communication/buffer error"},
		{e1&0xe8 != 0 || e2&0x80 != 0, driver.Unknown, fmt.Sprintf("error bits %02x %02x", e1, e2)},
	} {
		if e.hit {
			s.Code, s.Detail = e.code, e.detail
			break
		}
	}
	var kind driver.MediaKind
	switch b[11] {
	case 0x0a:
		kind = driver.Continuous
	case 0x0b:
		kind = driver.DieCut
	}
	if kind != "" {
		if m, ok := qlMediaBySize(model, kind, int(b[10]), int(b[17])); ok {
			s.Media = &m
		} else {
			s.Media = &driver.Media{ID: "unknown", Kind: kind, WidthMM: float64(b[10]), LengthMM: float64(b[17])}
		}
	}
	s.Done = b[18] == 0x01
	if b[18] == 0x02 && s.Code == driver.OK {
		s.Code = driver.Unknown
	}
	return s, nil
}
