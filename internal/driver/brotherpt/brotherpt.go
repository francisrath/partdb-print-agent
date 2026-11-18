// Package brotherpt implements the Brother P-touch PT-9500PC raster protocol, clean-room from
// Brother's public "PT-9500PC Command Reference, CBP-RASTER Mode (PTCBP Mode) Volume" (v1.0, 2003).
// This is an older dialect than the QL and PT-P700/P900 references (ESC i c, no compression,
// no model byte). No GPL code (ptouch-print, brother_ql) was used.
package brotherpt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) ID() string { return "brother-pt" }

func (d *Driver) Capabilities(modelName string) (driver.Capabilities, error) {
	if modelName != model {
		return driver.Capabilities{}, &driver.Error{Code: driver.Unsupported, Detail: "unknown Brother PT model " + modelName}
	}
	c := driver.Capabilities{DPI: dpi, HeadDots: headPins, Cutter: true}
	for _, m := range mediaTable {
		c.Media = append(c.Media, m.Media)
	}
	return c, nil
}

func (d *Driver) Validate(pages []*image.Gray, job *driver.Job, media *driver.Media) error {
	spec, ok := lookupMedia(media.ID)
	if !ok {
		return &driver.Error{Code: driver.Unsupported, Detail: "unknown tape " + media.ID}
	}
	if len(pages) == 0 {
		return errors.New("no pages")
	}
	if _, ok := cutModes[job.Cut]; !ok {
		return fmt.Errorf("unknown cut mode %q", job.Cut)
	}
	for i, p := range pages {
		w, h := p.Bounds().Dx(), p.Bounds().Dy()
		if w > spec.PrintableWidthDots {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots wide, %s prints %d", i+1, w, spec.ID, spec.PrintableWidthDots)}
		}
		if h == 0 || h > maxLines {
			return &driver.Error{Code: driver.Unsupported,
				Detail: fmt.Sprintf("page %d is %d dots long; tape allows 1–%d (1 m)", i+1, h, maxLines)}
		}
	}
	return nil
}

// cutModes maps job.Cut to the "set each mode" (ESC i M) and "set expanded mode" (ESC i K)
// bytes (p.19–20). K bit 3 (no chain printing) stays set: the last label is fed and cut.
// ponytail: last/none/half are from the reference's terse bit list; confirm on hardware.
var cutModes = map[string][2]byte{
	"":          {0x40, 0x08}, // auto cut each label
	"per_label": {0x40, 0x08},
	"last":      {0x00, 0x28}, // no auto cut, label end cut
	"none":      {0x00, 0x08},
	"half":      {0x40, 0x0c}, // auto cut + half cut (laminated tape only)
}

// EncodePages returns the job header and one block per page. The PT-9500PC wants each page
// sent only after the previous one reported "printing completed" (p.2).
func (d *Driver) EncodePages(pages []*image.Gray, job *driver.Job, media *driver.Media) ([]byte, [][]byte, error) {
	if err := d.Validate(pages, job, media); err != nil {
		return nil, nil, err
	}
	spec, _ := lookupMedia(media.ID)
	mk := cutModes[job.Cut]

	var h bytes.Buffer
	h.Write(make([]byte, invalidateN))
	h.Write([]byte{0x1b, '@'})
	// print information: valid flags width (0x04) + recover (0xF0, p.22); kind and length
	// stay unchecked so a type mismatch doesn't stop the job (we check the width ourselves)
	h.Write([]byte{0x1b, 'i', 'c', 0xf4, 0, byte(spec.WidthMM), 0, 0})
	h.Write([]byte{0x1b, 'i', 'M', mk[0]})
	h.Write([]byte{0x1b, 'i', 'K', mk[1]})
	h.Write([]byte{0x1b, 'i', 'd', margin & 0xff, margin >> 8})
	h.Write([]byte{'M', 0})            // no compression (TIFF is "invalid" on this model, p.23)
	h.Write([]byte{0x1b, 'i', 'R', 1}) // raster graphics mode

	lineBytes := (spec.offsetPins + spec.PrintableWidthDots + 7) / 8
	out := make([][]byte, len(pages))
	for i, p := range pages {
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
			line, blank := packLine(p, y, spec, lineBytes)
			if blank {
				b.WriteByte('Z')
				continue
			}
			b.Write([]byte{'G', byte(len(line)), byte(len(line) >> 8)})
			b.Write(line)
		}
		if i == len(pages)-1 {
			b.WriteByte(0x1a) // print with feed
		} else {
			b.WriteByte(0x0c) // print
		}
		out[i] = b.Bytes()
	}
	return h.Bytes(), out, nil
}

// packLine maps image row y onto head pins from pin 0 (MSB of the first byte, p.10–11).
// Image column x lands on pin offsetPins+x; narrower pages are centred.
// ponytail: orientation unverified until a label is printed; flip to (w-1-x) if mirrored.
func packLine(p *image.Gray, y int, spec mediaSpec, lineBytes int) ([]byte, bool) {
	line := make([]byte, lineBytes)
	r := p.Bounds()
	w := r.Dx()
	off := spec.offsetPins + (spec.PrintableWidthDots-w)/2
	row := p.Pix[y*p.Stride : y*p.Stride+w]
	blank := true
	for x, v := range row {
		if v < 128 {
			pin := off + x
			line[pin/8] |= 0x80 >> (pin % 8)
			blank = false
		}
	}
	return line, blank
}

func (d *Driver) Encode(ctx context.Context, w io.Writer, pages []*image.Gray, job *driver.Job, media *driver.Media) error {
	header, per, err := d.EncodePages(pages, job, media)
	if err != nil {
		return err
	}
	_, err = w.Write(bytes.Join(append([][]byte{header}, per...), nil))
	return err
}

var statusRequest = append(make([]byte, invalidateN), 0x1b, '@', 0x1b, 'i', 'S')

// Status asks for the 32-byte status. Never call it while the printer is printing (p.18).
func (d *Driver) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if _, err := t.Write(statusRequest); err != nil {
		return nil, err
	}
	s, _, err := readStatus(t)
	return s, err
}

func (d *Driver) Probe(ctx context.Context, t driver.Transport) (*driver.DeviceInfo, error) {
	s, err := d.Status(ctx, t)
	if err != nil {
		return nil, err
	}
	return &driver.DeviceInfo{Model: s.Model, Media: s.Media}, nil
}

// WaitPage reads statuses until "printing completed", skipping phase changes (the printer
// sends those while it prints, also before the page is fully sent, p.17). An error status
// returns a driver.Error; no completion within timeout returns driver.ErrNoReadback.
func WaitPage(ctx context.Context, t driver.Transport, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return driver.ErrNoReadback
		}
		s, typ, err := readStatus(t)
		if errors.Is(err, driver.ErrNoReadback) && !errors.Is(err, io.EOF) {
			continue // read timed out: still printing
		}
		if err != nil {
			return err
		}
		switch {
		case s.Code != driver.OK:
			return &driver.Error{Code: s.Code, Detail: s.Detail}
		case typ == statusCompleted:
			return nil
		}
	}
}

func readStatus(t driver.Transport) (*driver.Status, byte, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(t, buf); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, driver.ErrNoReadback) {
			return nil, 0, fmt.Errorf("%w: %w", driver.ErrNoReadback, err) // keep the cause: EOF means the device is gone
		}
		return nil, 0, err
	}
	return parseStatus(buf)
}
