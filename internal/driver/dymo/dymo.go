// Package dymo drives DYMO LabelWriter 300/400/450-series printers (and the 4XL) over their raster
// protocol, clean-room from DYMO's public "LabelWriter 400 Series" and "LabelWriter 450 Series Printers
// Technical Reference Manual"s. Dialects:
//   - "lw400": 400/450 series and 4XL (ESC h text mode, ESC c/e/g density, ETB compressed lines)
//   - "lw300": 330 / 330 Turbo (plain SYN raster lines only; assumed, no tech ref found)
//
// Untested on hardware: no DYMO printer was on hand.
package dymo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

func init() { driver.Register("dymo", New) }

const (
	esc = 0x1b
	syn = 0x16 // uncompressed raster line
	etb = 0x17 // run-length compressed raster line
	// die-cut: ESC L is the farthest the printer feeds looking for the next label's sense hole; the tech
	// ref says set it "slightly longer than the true label length". 60 lines = 5 mm.
	labelSlack = 60
)

type dymo struct {
	model *devicedb.Model
	opts  driver.Options
}

// New builds the driver for a DYMO model.
func New(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
	switch m.Dialect {
	case "lw400", "lw300":
	default:
		return nil, &driver.Error{Code: driver.Unsupported, Detail: fmt.Sprintf("%s: unknown DYMO dialect %q", m.ID, m.Dialect)}
	}
	if m.HeadPins <= 0 || m.HeadPins%8 != 0 {
		return nil, &driver.Error{Code: driver.Unsupported, Detail: fmt.Sprintf("%s: head_pins must be a positive multiple of 8", m.ID)}
	}
	return &dymo{model: m, opts: o}, nil
}

var statusRequest = []byte{esc, 'A'}

// Status asks with ESC A; the printer answers one byte. Silence (a unidirectional USB interface, or a
// 300-series printer that doesn't know ESC A) is driver.ErrNoReadback: printing goes on blind.
func (d *dymo) Status(_ context.Context, t driver.Transport) (*driver.Status, error) {
	if _, err := t.Write(statusRequest); err != nil {
		return nil, err
	}
	b, err := readByte(t)
	if err != nil {
		return nil, err
	}
	s := parseStatus(b)
	s.Model, s.Via = d.model.ID, t.Kind()
	return s, nil
}

func readByte(t driver.Transport) (byte, error) {
	var b [1]byte
	for {
		n, err := t.Read(b[:])
		if n == 1 {
			return b[0], nil
		}
		switch {
		case err == nil:
			continue // (0, nil): keep reading; the transport's timeout ends the wait
		case errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, driver.ErrNoReadback):
			return 0, fmt.Errorf("%w: %w", driver.ErrNoReadback, err)
		}
		return 0, err
	}
}

// parseStatus reads the ESC A status byte: bit 1 top of form, bit 5 no paper, bit 6 paper jam, bit 7 error
// (also set with no paper). Bit 0 is "ready" in the 400/450 references but inverted in the SE450's, so
// it is ignored.
func parseStatus(b byte) *driver.Status {
	s := &driver.Status{Code: driver.OK, Raw: fmt.Sprintf("%02x", b)}
	switch {
	case b&0x40 != 0:
		s.Code, s.Detail = driver.PaperJam, "paper jam"
	case b&0x20 != 0:
		s.Code, s.Detail = driver.NoMedia, "out of labels"
	case b&0x80 != 0:
		s.Code, s.Detail = driver.Unknown, "printer error (jam or invalid command sequence)"
	}
	s.Idle = s.Code == driver.OK && b&0x02 != 0 // at top of form: nothing half printed
	return s
}

// width is the dots a page may use: the media's printable width, never more than the head.
func (d *dymo) width(m *devicedb.Media) int {
	return min(m.OffsetPins+m.PrintableWidthDots, d.model.HeadPins) - m.OffsetPins
}

func (d *dymo) validate(job *driver.Job) error {
	if len(job.Pages) == 0 {
		return errors.New("no pages")
	}
	switch job.Cut {
	case "", "none":
	default:
		return &driver.Error{Code: driver.Unsupported, Detail: fmt.Sprintf("%s has no cutter (tear bar): cut %q", d.model.ID, job.Cut)}
	}
	m := job.Media
	for i, p := range job.Pages {
		w, h := p.Bounds().Dx(), p.Bounds().Dy()
		if w > d.width(m) {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots wide, %s on %s prints %d", i+1, w, m.ID, d.model.ID, d.width(m))}
		}
		if h == 0 || (m.Kind == devicedb.DieCut && h > m.PrintableLengthDots) {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots long, %s allows %d", i+1, h, m.ID, m.PrintableLengthDots)}
		}
	}
	return nil
}

// Encode writes the job: resync, line format, label length, mode and density, the raster lines of
// each label, ESC G between labels and ESC E (feed to the tear bar) after the last.
// ESC @ (reset) is never sent: it drops data still buffered, i.e. a previous job still printing.
func (d *dymo) Encode(w io.Writer, job *driver.Job) error {
	if err := d.validate(job); err != nil {
		return err
	}
	m := job.Media
	bpl := (m.OffsetPins + d.width(m) + 7) / 8
	lw400 := d.model.Dialect == "lw400"

	var b bytes.Buffer
	// "at least 85 continuous <esc> characters" (one more than the longest raster line) get the printer
	// out of a half-received line; 157 for the 4XL's 156-byte lines
	b.Write(bytes.Repeat([]byte{esc}, d.model.HeadPins/8+1))
	b.Write([]byte{esc, 'B', 0, esc, 'D', byte(bpl)})
	length := uint16(0xffff) // negative: continuous
	if m.Kind == devicedb.DieCut {
		length = uint16(m.PrintableLengthDots + labelSlack)
	}
	b.Write([]byte{esc, 'L', byte(length >> 8), byte(length)})
	if lw400 {
		b.Write([]byte{esc, 'h'}) // 300×300 dpi text mode: the renderer's dots
		switch d.opts.Darkness {
		case "light":
			b.Write([]byte{esc, 'c'})
		case "dark":
			b.Write([]byte{esc, 'g'})
		default:
			b.Write([]byte{esc, 'e'})
		}
	}
	for i, p := range job.Pages {
		for y := range p.Bounds().Dy() {
			line := packLine(p, y, m.OffsetPins, bpl)
			if c := compress(line); lw400 && len(c) < len(line) {
				b.WriteByte(etb)
				b.Write(c)
			} else {
				b.WriteByte(syn)
				b.Write(line)
			}
		}
		if i < len(job.Pages)-1 {
			b.Write([]byte{esc, 'G'}) // next label into print position
		} else {
			b.Write([]byte{esc, 'E'}) // last label out to the tear bar
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// packLine maps image row y onto bpl bytes: byte 0's MSB is the leftmost dot (the label's left edge;
// ESC B "shifts the starting dot position … towards the right").
func packLine(p *image.Gray, y, off, bpl int) []byte {
	line := make([]byte, bpl)
	row := p.Pix[y*p.Stride : y*p.Stride+p.Bounds().Dx()]
	for x, v := range row {
		if v < 128 {
			dot := off + x
			line[dot/8] |= 0x80 >> (dot % 8)
		}
	}
	return line
}

// compress is the tech ref's Appendix A run-length code: one byte per run of up to 128 dots of one
// colour, bit 7 = black, bits 0-6 = run length - 1. The runs cover exactly len(line)*8 dots.
func compress(line []byte) []byte {
	var out []byte
	bit := func(i int) byte { return line[i/8] >> (7 - i%8) & 1 }
	n := len(line) * 8
	for i := 0; i < n; {
		v, j := bit(i), i+1
		for j < n && j-i < 128 && bit(j) == v {
			j++
		}
		out = append(out, v<<7|byte(j-i-1))
		i = j
	}
	return out
}

// Print sends the job. DYMO printers report no completion; when the printer answered ESC A before, one
// more ESC A after sending catches a jam or an empty roll, but never confirms the label printed.
func (d *dymo) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
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
	if job.Status == nil || job.Status.Via == "" {
		return res, nil
	}
	s, err := d.Status(ctx, t)
	switch {
	case errors.Is(err, driver.ErrNoReadback):
		res.Message = "sent (unconfirmed: no status after sending)"
	case err != nil:
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	case s.Code != driver.OK:
		return nil, &driver.Error{Code: s.Code, Detail: s.Detail}
	default:
		res.Message = "sent (unconfirmed: DYMO printers report no completion; no error)"
	}
	return res, nil
}
