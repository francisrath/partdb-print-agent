// Package brotherql implements the Brother QL raster protocol, clean-room from Brother's
// public "Raster Command Reference" (QL-800/810W/820NWB). No brother_ql (GPL) code.
package brotherql

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

type Driver struct {
	// NoCompression disables TIFF/PackBits raster compression.
	NoCompression bool
}

func New() *Driver { return &Driver{} }

func (d *Driver) ID() string { return "brother-ql" }

func (d *Driver) Capabilities(modelName string) (driver.Capabilities, error) {
	if _, ok := findModel(modelName); !ok {
		return driver.Capabilities{}, &driver.Error{Code: driver.Unsupported, Detail: "unknown Brother QL model " + modelName}
	}
	c := driver.Capabilities{DPI: dpi, HeadDots: headPins, Cutter: true}
	for _, m := range mediaTable {
		c.Media = append(c.Media, m.Media)
	}
	return c, nil
}

func findModel(name string) (model, bool) {
	for _, m := range models {
		if m.name == name {
			return m, true
		}
	}
	return model{}, false
}

func (d *Driver) Validate(pages []*image.Gray, job *driver.Job, media *driver.Media) error {
	spec, ok := lookupMedia(media.ID)
	if !ok {
		return &driver.Error{Code: driver.Unsupported, Detail: "unknown media " + media.ID}
	}
	if len(pages) == 0 {
		return errors.New("no pages")
	}
	switch job.Cut {
	case "", "per_label", "last", "none":
	default:
		return fmt.Errorf("unknown cut mode %q", job.Cut)
	}
	for i, p := range pages {
		w, h := p.Bounds().Dx(), p.Bounds().Dy()
		if w > spec.PrintableWidthDots {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots wide, %s prints %d", i+1, w, spec.ID, spec.PrintableWidthDots)}
		}
		if h == 0 || (spec.Kind == driver.DieCut && h > spec.PrintableLengthDots) {
			return &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d dots long, %s allows %d", i+1, h, spec.ID, spec.PrintableLengthDots)}
		}
	}
	return nil
}

func (d *Driver) Encode(ctx context.Context, w io.Writer, pages []*image.Gray, job *driver.Job, media *driver.Media) error {
	if err := d.Validate(pages, job, media); err != nil {
		return err
	}
	spec, _ := lookupMedia(media.ID)

	var b bytes.Buffer
	b.Write(make([]byte, invalidateN))
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
	kind, flags, margin := byte(0x0a), byte(0xce), uint16(feedMargin)
	if spec.Kind == driver.DieCut {
		kind, margin = 0x0b, 0
	}

	for i, p := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		lines := p.Bounds().Dy()
		padTop, padBottom := 0, 0
		if lines < minLines { // short continuous label: centre it in the minimum length
			padTop = (minLines - lines) / 2
			padBottom = minLines - lines - padTop
		}
		start := byte(1)
		if i == 0 {
			start = 0
		}
		b.Write([]byte{0x1b, 'i', 'z', flags, kind, byte(spec.WidthMM), byte(spec.LengthMM)})
		b.Write(binary.LittleEndian.AppendUint32(nil, uint32(padTop+lines+padBottom)))
		b.Write([]byte{start, 0})
		b.Write([]byte{0x1b, 'i', 'M', various})
		if various&0x40 != 0 {
			b.Write([]byte{0x1b, 'i', 'A', 1}) // cut every label
		}
		b.Write([]byte{0x1b, 'i', 'K', expanded})
		b.Write(binary.LittleEndian.AppendUint16([]byte{0x1b, 'i', 'd'}, margin))
		if d.NoCompression {
			b.Write([]byte{'M', 0})
		} else {
			b.Write([]byte{'M', 2})
		}
		blank := make([]byte, lineBytes)
		for y := -padTop; y < lines+padBottom; y++ {
			line := blank
			if y >= 0 && y < lines {
				line = packLine(p, y, spec)
			}
			if !d.NoCompression {
				line = packbits(line)
			}
			b.Write([]byte{'g', 0, byte(len(line))})
			b.Write(line)
		}
		if i == len(pages)-1 {
			b.WriteByte(0x1a) // print with feed
		} else {
			b.WriteByte(0x0c) // print
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// packLine maps image row y onto the 720-pin head. Pins are numbered from the
// spec's "right" edge, so the image's rightmost column lands on pin rightPins.
func packLine(p *image.Gray, y int, spec mediaSpec) []byte {
	line := make([]byte, lineBytes)
	r := p.Bounds()
	w := r.Dx()
	off := spec.rightPins + (spec.PrintableWidthDots-w)/2 // centre narrower pages
	row := p.Pix[(y)*p.Stride : (y)*p.Stride+w]
	for x, v := range row {
		if v < 128 {
			pin := off + (w - 1 - x)
			line[pin/8] |= 0x80 >> (pin % 8)
		}
	}
	return line
}

// packbits is TIFF PackBits: n>=0 → n+1 literal bytes; n<0 → next byte repeated 1-n times.
func packbits(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); {
		j := i + 1
		for j < len(src) && src[j] == src[i] && j-i < 128 {
			j++
		}
		if j-i >= 2 {
			out = append(out, byte(257-(j-i)), src[i])
			i = j
			continue
		}
		j = i + 1
		for j < len(src) && j-i < 128 && !(j+1 < len(src) && src[j] == src[j+1]) {
			j++
		}
		out = append(out, byte(j-i-1))
		out = append(out, src[i:j]...)
		i = j
	}
	return out
}

var statusRequest = append(make([]byte, invalidateN), 0x1b, '@', 0x1b, 'i', 'S')

func (d *Driver) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if _, err := t.Write(statusRequest); err != nil {
		return nil, err
	}
	return readStatus(t)
}

func (d *Driver) Probe(ctx context.Context, t driver.Transport) (*driver.DeviceInfo, error) {
	s, err := d.Status(ctx, t)
	if err != nil {
		return nil, err
	}
	return &driver.DeviceInfo{Model: s.Model, Media: s.Media}, nil
}

// WaitDone reads status replies until n pages report "printing completed" or an error arrives.
// Only call it after Status succeeded on t; returns driver.ErrNoReadback on timeout.
func WaitDone(ctx context.Context, t driver.Transport, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for done := 0; done < n; {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, err := readStatus(t)
		if errors.Is(err, driver.ErrNoReadback) && time.Now().Before(deadline) {
			continue // long label still printing
		}
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

func readStatus(t driver.Transport) (*driver.Status, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(t, buf); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, driver.ErrNoReadback) {
			return nil, driver.ErrNoReadback
		}
		return nil, err
	}
	return parseStatus(buf)
}

func parseStatus(b []byte) (*driver.Status, error) {
	if len(b) != 32 || b[0] != 0x80 || b[1] != 0x20 || b[2] != 'B' {
		return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("bad status reply % x", b)}
	}
	s := &driver.Status{Code: driver.OK, Model: fmt.Sprintf("unknown (0x%02x)", b[4])}
	for _, m := range models {
		if m.code == b[4] {
			s.Model = m.name
		}
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
		if m, ok := mediaBySize(kind, int(b[10]), int(b[17])); ok {
			s.Media = &m.Media
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
