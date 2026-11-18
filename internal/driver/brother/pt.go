package brother

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// pt-cbp dialect: Brother's "PT-9500PC Command Reference, CBP-RASTER Mode (PTCBP Mode)
// Volume", v1.0, 2003 (page numbers in comments are that document's). Older than the QL and
// PT-P700/P900 references: ESC i c print info, no compression, no model byte, and each page
// is sent only after the previous one reported "printing completed" (p.2).

const ptInvalidateN = 100

// Status types (byte 18, p.16).
const (
	ptStatusReply     = 0x00
	ptStatusCompleted = 0x01
	ptStatusError     = 0x02
)

// ptPageTimeout bounds the wait for one label's "printing completed": a 1 m label at the
// PT-9500PC's ~20 mm/s takes about 50 s.
const ptPageTimeout = 90 * time.Second

var ptMediaTypes = map[byte]string{0x01: "laminated", 0x02: "lettering", 0x03: "non-laminated", 0x08: "av", 0x09: "hg"}

type pt struct {
	model *devicedb.Model
	media []driver.Media
}

func newPT(m *devicedb.Model) *pt {
	return &pt{model: m, media: devicedb.Default().MediaFor(m)}
}

func (d *pt) mediaForWidth(mm int) (driver.Media, bool) {
	for _, m := range d.media {
		if int(m.WidthMM) == mm {
			return m, true
		}
	}
	return driver.Media{}, false
}

var ptStatusRequest = append(make([]byte, ptInvalidateN), 0x1b, '@', 0x1b, 'i', 'S')

// Status asks for the 32-byte status. Never call it while the printer is printing (p.18).
// Statuses still queued from an earlier job (phase change, printing completed) are skipped,
// so they can't pass for this reply or confirm a page of the next job.
func (d *pt) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if _, err := t.Write(ptStatusRequest); err != nil {
		return nil, err
	}
	for range 16 {
		b, err := readReply(t)
		if err != nil {
			return nil, err
		}
		s, typ, err := d.parseStatus(b)
		if err != nil || typ == ptStatusReply {
			return s, err
		}
	}
	return nil, &driver.Error{Code: driver.CommsError, Detail: "no reply to the status request among the printer's messages"}
}

// parseStatus decodes the 32-byte status (p.14–17). The PT-9500PC has no model byte.
func (d *pt) parseStatus(b []byte) (*driver.Status, byte, error) {
	if len(b) != 32 || b[0] != 0x80 || b[1] != 0x20 || b[2] != 'B' {
		return nil, 0, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("bad status reply % x", b)}
	}
	s := &driver.Status{Code: driver.OK, Model: d.model.ID, MediaType: ptMediaTypes[b[11]]}
	e1, e2 := b[8], b[9]
	for _, e := range []struct {
		hit    bool
		code   driver.Code
		detail string
	}{
		{e1&0x01 != 0, driver.NoMedia, "no media"},
		{e1&0x02 != 0, driver.NoMedia, "end of media"},
		{e1&0x04 != 0, driver.CutterJam, "cutter jam"},
		{e2&0x01 != 0, driver.MediaMismatch, "replace media"},
		{e2&0x10 != 0, driver.CoverOpen, "cover open"},
		{e2&0x0e != 0, driver.CommsError, fmt.Sprintf("communication/buffer error %02x", e2)},
		{e1&0xf8 != 0 || e2&0xe0 != 0, driver.Unknown, fmt.Sprintf("error bits %02x %02x", e1, e2)},
	} {
		if e.hit {
			s.Code, s.Detail = e.code, e.detail
			break
		}
	}
	if w := int(b[10]); w != 0 {
		if m, ok := d.mediaForWidth(w); ok {
			s.Media = &m
		} else {
			s.Media = &driver.Media{ID: "unknown", Kind: driver.Continuous, WidthMM: float64(w)}
		}
	}
	typ := b[18]
	s.Done = typ == ptStatusCompleted
	if typ == ptStatusError && s.Code == driver.OK {
		s.Code = driver.Unknown
	}
	return s, typ, nil
}

// cutBytes returns the [ESC i M, ESC i K] bytes for a cut mode from the model's quirks.
func (d *pt) cutBytes(cut string) ([2]byte, bool) {
	modes, _ := d.model.Quirks["cut_modes"].(map[string]any)
	v, ok := modes[cut].([]any)
	if !ok || len(v) != 2 {
		return [2]byte{}, false
	}
	m, mok := v[0].(int)
	k, kok := v[1].(int)
	return [2]byte{byte(m), byte(k)}, mok && kok
}

// resolveCut picks the cut mode. Default: a batch on laminated tape is one half-cut strip,
// anything else is cut label by label ("chain"); either way the last label waits in the
// printer until the next print (or the cut button) instead of wasting a blank leader.
// Half cut only works on laminated tape (p.20); without a status (dry run) the tape is
// unknown, so an explicit "half" is kept.
func (d *pt) resolveCut(job *driver.Job) (cut, warning string) {
	def := d.model.Defaults["cut"]
	if def == "" {
		def = "chain"
	}
	laminated := job.Status != nil && (job.Status.MediaType == "laminated" || job.Status.MediaType == "hg")
	switch {
	case job.Cut == "" && len(job.Pages) > 1 && laminated:
		return "half", ""
	case job.Cut == "":
		return def, ""
	case job.Cut == "half" && !laminated && job.Status != nil:
		return def, "half cut needs laminated tape; cut between labels instead"
	}
	return job.Cut, ""
}

func (d *pt) validate(job *driver.Job) error {
	m := job.Media
	if m == nil {
		return errors.New("no media")
	}
	known := false
	for _, md := range d.media {
		known = known || md.ID == m.ID
	}
	if !known {
		return &driver.Error{Code: driver.Unsupported, Detail: "unknown tape " + m.ID}
	}
	if len(job.Pages) == 0 {
		return errors.New("no pages")
	}
	if job.Cut == "last" {
		return fmt.Errorf(`cut "last" isn't possible on the %s: labels are sent one at a time, so the printer can't tell which is last (use half or chain)`, d.model.ID)
	}
	if _, ok := d.cutBytes(job.Cut); job.Cut != "" && !ok {
		return fmt.Errorf("unknown cut mode %q", job.Cut)
	}
	maxLines := d.model.Quirk("max_lines", 14173)
	for i, p := range job.Pages {
		w, h := p.Bounds().Dx(), p.Bounds().Dy()
		if w > m.PrintableWidthDots {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots wide, %s prints %d", i+1, w, m.ID, m.PrintableWidthDots)}
		}
		if h == 0 || h > maxLines {
			return &driver.Error{Code: driver.Unsupported,
				Detail: fmt.Sprintf("page %d is %d dots long; tape allows 1–%d (1 m)", i+1, h, maxLines)}
		}
	}
	return nil
}

// encodePages returns the job header and one block per page (p.4, p.18–23).
func (d *pt) encodePages(job *driver.Job) ([]byte, [][]byte, error) {
	if err := d.validate(job); err != nil {
		return nil, nil, err
	}
	cut, _ := d.resolveCut(job)
	mk, ok := d.cutBytes(cut)
	if !ok {
		return nil, nil, fmt.Errorf("%s has no cut mode %q", d.model.ID, cut)
	}
	m := job.Media
	margin := d.model.Quirk("margin", 28)
	minLines := d.model.Quirk("min_lines", 57)

	var h bytes.Buffer
	h.Write(make([]byte, ptInvalidateN))
	h.Write([]byte{0x1b, '@'})
	// print information: valid flags width (0x04) + recover (0xF0, p.22); kind and length
	// stay unchecked so a type mismatch doesn't stop the job (we check the width ourselves)
	h.Write([]byte{0x1b, 'i', 'c', 0xf4, 0, byte(m.WidthMM), 0, 0})
	h.Write([]byte{0x1b, 'i', 'M', mk[0]})
	h.Write([]byte{0x1b, 'i', 'K', mk[1]})
	h.Write([]byte{0x1b, 'i', 'd', byte(margin), byte(margin >> 8)})
	h.Write([]byte{'M', 0})            // no compression (TIFF is "invalid" on this model, p.23)
	h.Write([]byte{0x1b, 'i', 'R', 1}) // raster graphics mode

	lineBytes := (m.OffsetPins + m.PrintableWidthDots + 7) / 8
	reversed := d.model.QuirkString("pin_order") == "reversed"
	out := make([][]byte, len(job.Pages))
	for i, p := range job.Pages {
		lines := p.Bounds().Dy()
		padTop, padBottom := 0, 0
		if lines < minLines { // short label: centre it in the 4 mm minimum
			padTop = (minLines - lines) / 2
			padBottom = minLines - lines - padTop
		}
		var b bytes.Buffer
		for y := -padTop; y < lines+padBottom; y++ {
			if y < 0 || y >= lines {
				b.WriteByte('Z')
				continue
			}
			line, blank := ptPackLine(p, y, *m, lineBytes, reversed)
			if blank {
				b.WriteByte('Z')
				continue
			}
			b.Write([]byte{'G', byte(len(line)), byte(len(line) >> 8)})
			b.Write(line)
		}
		if i == len(job.Pages)-1 {
			b.WriteByte(0x1a) // print with feed
		} else {
			b.WriteByte(0x0c) // print
		}
		out[i] = b.Bytes()
	}
	return h.Bytes(), out, nil
}

// ptPackLine maps image row y onto head pins from pin 0 (MSB of the first byte, p.10–11);
// narrower pages are centred. With reversed pin order (PT-9500PC: x on pin offset+x printed
// mirrored, 2026-11-18) the image's rightmost column lands on the first print-area pin.
func ptPackLine(p *image.Gray, y int, m driver.Media, lineBytes int, reversed bool) ([]byte, bool) {
	line := make([]byte, lineBytes)
	w := p.Bounds().Dx()
	off := m.OffsetPins + (m.PrintableWidthDots-w)/2
	row := p.Pix[y*p.Stride : y*p.Stride+w]
	blank := true
	for x, v := range row {
		if v < 128 {
			pin := off + x
			if reversed {
				pin = off + (w - 1 - x)
			}
			line[pin/8] |= 0x80 >> (pin % 8)
			blank = false
		}
	}
	return line, blank
}

func (d *pt) Encode(w io.Writer, job *driver.Job) error {
	header, per, err := d.encodePages(job)
	if err != nil {
		return err
	}
	_, err = w.Write(bytes.Join(append([][]byte{header}, per...), nil))
	return err
}

// Print sends the header, then each label once the previous one reported "printing
// completed" (p.2). It needs the pre-print status: without read-back it can't pace.
func (d *pt) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	if job.Status == nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: "printer sent no status reply"}
	}
	cut, warning := d.resolveCut(job)
	header, per, err := d.encodePages(job)
	if err != nil {
		return nil, err
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: len(header)}
	for _, pg := range per {
		res.Bytes += len(pg)
	}
	if warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	if _, err := t.Write(header); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	for i, pg := range per {
		if err := ctx.Err(); err != nil {
			return nil, err // don't start a label after cancellation
		}
		if _, err := t.Write(pg); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		err := d.waitPage(ctx, t, ptPageTimeout)
		var de *driver.Error
		switch {
		case errors.As(err, &de):
			return nil, de
		case errors.Is(err, driver.ErrNoReadback) && i == len(per)-1:
			res.Message = "sent (unconfirmed: no completion status received)"
			return res, nil
		case errors.Is(err, driver.ErrNoReadback):
			// sending more while it may still print could damage the printer (p.2)
			return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("label %d: printer never reported it printed", i+1)}
		case err != nil:
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
	}
	res.Confirmed, res.Message = true, "printed (cut "+cut+")"
	return res, nil
}

// waitPage reads statuses until "printing completed", skipping phase changes (the printer
// sends those while it prints, also before the page is fully sent, p.17). An error status
// returns a driver.Error; no completion within timeout returns driver.ErrNoReadback.
func (d *pt) waitPage(ctx context.Context, t driver.Transport, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	r := &replyReader{t: t} // keeps a partial reply until the deadline
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			if r.n > 0 {
				return r.incomplete(driver.ErrNoReadback)
			}
			return driver.ErrNoReadback
		}
		b, err := r.read()
		if errors.Is(err, driver.ErrNoReadback) && !errors.Is(err, io.EOF) {
			continue // read timed out: still printing (or mid-reply)
		}
		if err != nil {
			return err
		}
		s, typ, err := d.parseStatus(b)
		if err != nil {
			return err
		}
		switch {
		case s.Code != driver.OK:
			return &driver.Error{Code: s.Code, Detail: s.Detail}
		case typ == ptStatusCompleted:
			return nil
		}
	}
}
