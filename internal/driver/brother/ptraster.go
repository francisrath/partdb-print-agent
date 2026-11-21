package brother

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

// pt-raster dialect: the raster mode of the newer P-touch printers, from Brother's "Raster Command
// Reference" for PT-E550W/P750W/P710BT (v1.02), PT-H500/P700/E500 (v1.11) and
// PT-P900/P900W/P950NW/P910BT (v1.02); page numbers below are the PT-E550W one's. The QL's command
// set (ESC i z, ESC i M/K/d, TIFF, 32-byte status, all pages sent at once) on a TZe head of 128
// pins at 180 dpi or 560 at 360 dpi, with the PT-9500PC's raster lines ("G" n1 n2, "Z") and cut
// modes. Only the standard resolution: no high-resolution (180×360, 360×720) or draft printing.
//
// untested: written from the references, 2026-11-20; no P-touch of this kind was at hand.

const ptRasterInvalidateN = 200 // the PT-P900 reference sends 200, the others 100

var ptRasterMediaTypes = map[byte]string{0x01: "laminated", 0x03: "non-laminated", 0x04: "fabric",
	0x11: "heat-shrink 2:1", 0x13: "FLe label", 0x14: "flexible ID", 0x15: "satin", 0x17: "heat-shrink 3:1", 0xff: "incompatible"}

// ptRasterTZe are the media types that are TZe tape of the reported width (PT-P900 reference p.27;
// heat-shrink tube reports a width too, FLe is a 21×45 mm die-cut label).
var ptRasterTZe = map[byte]bool{0x01: true, 0x03: true, 0x04: true, 0x14: true, 0x15: true}

// ptRaster pairs the PT-9500PC's tape and cut handling with the QL's status and confirmation.
type ptRaster struct {
	tape *pt
	q    *ql
}

func newPTRaster(m *devicedb.Model, o driver.Options) *ptRaster {
	return &ptRaster{tape: newPT(m), q: newQL(m, o)}
}

// Status reads the printer over SNMP on the network, else with ESC i S (p.23). The PT-E550W
// reference's command list marks ESC i S unsupported on the PT-E550W/P750W while documenting
// their model codes in its reply; over USB we ask anyway.
func (d *ptRaster) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	return d.q.status(ctx, t, d.parse)
}

// parse decodes the 32-byte status (p.24–30; the PT-P900 adds battery and extended error bytes
// at 6–7, unused here).
func (d *ptRaster) parse(b []byte) (*driver.Status, error) {
	if len(b) != 32 || b[0] != 0x80 || b[1] != 0x20 || b[2] != 'B' {
		return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("bad status reply % x", b)}
	}
	s := &driver.Status{Code: driver.OK, Model: d.tape.model.ID, MediaType: ptRasterMediaTypes[b[11]]}
	if m, ok := devicedb.Default().ModelByStatusCode("brother", int(b[4])); ok {
		s.Model = m.ID
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
		{e1&0x08 != 0, driver.Unknown, "weak batteries"},
		{e1&0x10 != 0, driver.Busy, "printer in use"},
		{e1&0x40 != 0, driver.Unknown, "high-voltage adapter"},
		{e2&0x01 != 0, driver.MediaMismatch, "replace media"},
		{e2&0x10 != 0, driver.CoverOpen, "cover open"},
		{e2&0x20 != 0, driver.Overheat, "overheating"},
		{e2&0x0e != 0, driver.CommsError, fmt.Sprintf("communication/buffer error %02x", e2)},
		{e1&0xa0 != 0 || e2&0xc0 != 0, driver.Unknown, fmt.Sprintf("error bits %02x %02x", e1, e2)},
	} {
		if e.hit {
			s.Code, s.Detail = e.code, e.detail
			break
		}
	}
	if w := int(b[10]); w != 0 {
		if m, ok := d.tape.mediaForWidth(w); ok && ptRasterTZe[b[11]] {
			s.Media = &m
		} else {
			s.Media = &devicedb.Media{ID: "unknown", Kind: devicedb.Continuous, WidthMM: float64(w)}
		}
	}
	s.Done = b[18] == ptStatusCompleted
	if b[18] == ptStatusError && s.Code == driver.OK {
		s.Code = driver.Unknown
	}
	return s, nil
}

// Encode writes the job: initialise, raster mode, then per page print information, mode
// settings, margin, compression, raster lines and the print command (p.5–7).
func (d *ptRaster) Encode(w io.Writer, job *driver.Job) error {
	header, pages, err := d.encodePages(job)
	if err != nil {
		return err
	}
	_, err = w.Write(bytes.Join(append([][]byte{header}, pages...), nil))
	return err
}

// encodePages returns the job header and one block per page.
func (d *ptRaster) encodePages(job *driver.Job) ([]byte, [][]byte, error) {
	if err := d.tape.validate(job); err != nil {
		return nil, nil, err
	}
	model := d.tape.model
	cut, _ := d.tape.resolveCut(job)
	mk, ok := d.tape.cutBytes(cut)
	if !ok {
		return nil, nil, fmt.Errorf("%s has no cut mode %q", model.ID, cut)
	}
	m := job.Media
	margin := model.Quirk("feed_margin", 14)
	minLines := model.Quirk("min_lines", 31)
	raw := model.QuirkBool("no_compression")
	lineBytes := model.HeadPins / 8

	var h bytes.Buffer
	h.Write(make([]byte, ptRasterInvalidateN))
	h.Write([]byte{0x1b, '@'})
	h.Write([]byte{0x1b, 'i', 'a', 1}) // raster mode
	if model.QuirkBool("status_notify") {
		h.Write([]byte{0x1b, 'i', '!', 0}) // PT-P710BT/P910BT: report status while printing
	}
	out := make([][]byte, len(job.Pages))
	for i, p := range job.Pages {
		var b bytes.Buffer
		lines := p.Bounds().Dy()
		padTop, padBottom := 0, 0
		if lines < minLines { // short label: centre it in the minimum length
			padTop = (minLines - lines) / 2
			padBottom = minLines - lines - padTop
		}
		page := byte(1)
		switch {
		case i == len(job.Pages)-1 && model.QuirkBool("marks_last_page"):
			page = 2 // PT-P900 series: last (or only) page
		case i == 0:
			page = 0
		}
		// print information: valid flags recover | width (0x84) with media type 0, as in each
		// reference's example; the tape type stays unchecked, the width is checked
		b.Write([]byte{0x1b, 'i', 'z', 0x84, 0, byte(mm(m.WidthMM)), 0})
		b.Write(binary.LittleEndian.AppendUint32(nil, uint32(padTop+lines+padBottom)))
		b.Write([]byte{page, 0})
		b.Write([]byte{0x1b, 'i', 'M', mk[0]})
		if mk[0]&0x40 != 0 && !model.QuirkBool("no_cut_count") {
			b.Write([]byte{0x1b, 'i', 'A', 1}) // cut every label
		}
		b.Write([]byte{0x1b, 'i', 'K', mk[1]})
		b.Write(binary.LittleEndian.AppendUint16([]byte{0x1b, 'i', 'd'}, uint16(margin)))
		if raw {
			b.Write([]byte{'M', 0})
		} else {
			b.Write([]byte{'M', 2})
		}
		blank := make([]byte, lineBytes)
		for y := -padTop; y < lines+padBottom; y++ {
			line, empty := blank, true
			if y >= 0 && y < lines {
				line, empty = ptPackLine(p, y, *m, lineBytes, true)
			}
			switch {
			case empty && !raw:
				b.WriteByte('Z') // zero raster: TIFF mode only
			case raw:
				b.Write([]byte{'G', byte(lineBytes), byte(lineBytes >> 8)})
				b.Write(line)
			default:
				c := packbits(line)
				b.Write([]byte{'G', byte(len(c)), byte(len(c) >> 8)})
				b.Write(c)
			}
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

// Print sends the job. When the printer answered the status request over its own connection
// (USB, Bluetooth) each label goes only after the previous one reported "printing completed"
// ("(2) through (4) are repeated", p.4, flow chart 5.1); one that gave no status there gets
// single labels only. Over the network the printer gives no status (flow chart 5.6): the whole
// job goes at once and SNMP confirms it, like a QL.
func (d *ptRaster) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	_, warning := d.tape.resolveCut(job)
	header, pages, err := d.encodePages(job)
	if err != nil {
		return nil, err
	}
	silent := job.Status == nil || job.Status.Via == ""
	if silent && t.Kind() != "tcp" && len(pages) > 1 {
		return nil, &driver.Error{Code: driver.CommsError, Detail: "the printer doesn't report status, so labels can't be sent " +
			"one at a time; print one label per job or check the connection"}
	}
	var res *driver.Result
	if st := job.Status; silent || st.Via == "snmp" {
		res, err = d.q.send(ctx, t, job, bytes.Join(append([][]byte{header}, pages...), nil), d.parse)
	} else {
		res, err = d.sendPaced(ctx, t, job, header, pages)
	}
	if res != nil && warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, err
}

// sendPaced writes the header with the first label, then each label after the previous one's
// "printing completed".
func (d *ptRaster) sendPaced(ctx context.Context, t driver.Transport, job *driver.Job, header []byte, pages [][]byte) (*driver.Result, error) {
	res := &driver.Result{Pages: len(pages), Media: job.Media.ID, Bytes: len(header)}
	for i, pg := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err // don't start a label after cancellation
		}
		if i == 0 {
			pg = append(header[:len(header):len(header)], pg...)
		}
		if _, err := t.Write(pg); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		res.Bytes += len(pages[i])
		err := waitDoneWith(ctx, t, 1, ptPageTimeout, d.parse)
		switch {
		case errors.Is(err, driver.ErrNoReadback) && i == len(pages)-1:
			res.Message = "sent (unconfirmed: no completion status received)"
			return res, nil
		case errors.Is(err, driver.ErrNoReadback):
			return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("label %d: printer never reported it printed", i+1)}
		case err != nil:
			return nil, err
		}
	}
	res.Message, res.Confirmed = "printed", true
	return res, nil
}
