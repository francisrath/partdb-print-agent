// Package phomemo drives Phomemo thermal printers over Bluetooth LE: ESC/POS GS v 0 raster framed per
// family (dialects m02: pocket printers, m110: label printers, d30: 12 mm label makers), status as
// `1A type value` notifications, a `1A 0F 0C` per printed page. Facts from MIT/Apache projects (see the
// design note); no vendor spec exists.
package phomemo

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"slices"
	"time"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

func init() {
	driver.Register("phomemo", func(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
		if !slices.Contains([]string{"m02", "m110", "d30"}, m.Dialect) {
			return nil, fmt.Errorf("phomemo: unknown dialect %q", m.Dialect)
		}
		// m110 density 1–15 (`1B 4E 04`); the others have no density setting we trust
		d, ok := map[string]byte{"light": 6, "": 10, "normal": 10, "dark": 15}[o.Darkness]
		if !ok {
			return nil, fmt.Errorf("phomemo: darkness %q must be light, normal or dark", o.Darkness)
		}
		return &phomemo{model: m, density: d}, nil
	})
}

// Timings; tests shorten them.
var (
	replyWait      = 1500 * time.Millisecond // for status replies
	confirmBase    = 15 * time.Second        // then "sent (not confirmed)"
	confirmPerPage = 5 * time.Second
)

type phomemo struct {
	model   *devicedb.Model
	density byte
}

// Status reply types (`1A type value`) and values.
const (
	replyTemp    = 0x03 // A8 normal, A9 too hot
	replyBattery = 0x04 // percent
	replyCover   = 0x05 // 98 closed, 99 open
	replyPaper   = 0x06 // 89 loaded, 88 out
	replyDone    = 0x0F // 0C = page printed, else failed
	doneOK       = 0x0C
)

// raster appends img as GS v 0 blocks of at most band rows (0 = one block), each row padded white to
// head dots: left, centred or right (align) when the image is narrower; dots past the head are cut off.
func raster(b []byte, img *image.Gray, head, band int, align string) []byte {
	r := img.Bounds()
	w := min(r.Dx(), head)
	off := 0
	switch align {
	case "right":
		off = head - w
	case "center":
		off = (head - w) / 2
	}
	wb := (head + 7) / 8
	if band <= 0 {
		band = max(r.Dy(), 1)
	}
	for y0 := 0; y0 < r.Dy(); y0 += band {
		n := min(band, r.Dy()-y0)
		b = append(b, 0x1D, 0x76, 0x30, 0x00, byte(wb), byte(wb>>8), byte(n), byte(n>>8))
		for y := y0; y < y0+n; y++ {
			row := make([]byte, wb)
			for x := range w {
				if img.GrayAt(r.Min.X+x, r.Min.Y+y).Y < 128 {
					row[(off+x)/8] |= 0x80 >> ((off + x) % 8)
				}
			}
			b = append(b, row...)
		}
	}
	return b
}

// page encodes one label in the model's framing.
func (p *phomemo) page(b []byte, img *image.Gray, media *devicedb.Media) []byte {
	m := p.model
	head, band, align := m.HeadPins, m.Quirk("band_rows", 0), m.QuirkString("align")
	switch m.Dialect {
	case "m02": // init, density, raster in short bands, feed
		b = append(b, 0x1B, 0x40, 0x1F, 0x11, 0x02, 0x04)
		b = raster(b, img, head, band, align)
		return append(b, 0x1B, 0x64, 0x02)
	case "m110": // speed, density, media type, raster, end of label (feeds to the next gap)
		kind := byte(0x0A) // labels with gaps
		if media != nil && media.Kind == devicedb.Continuous {
			kind = 0x0B
		}
		b = append(b, 0x1B, 0x4E, 0x0D, byte(m.Quirk("speed", 3)), 0x1B, 0x4E, 0x04, p.density, 0x1F, 0x11, kind)
		b = raster(b, img, head, band, align)
		return append(b, 0x1F, 0xF0, 0x05, 0x00, 0x1F, 0xF0, 0x03, 0x00)
	default: // d30: no margin, init, one raster, no feed
		b = append(b, 0x1F, 0x11, 0x24, 0x00, 0x1B, 0x40)
		b = raster(b, img, head, band, align)
		return append(b, 0x1B, 0x64, 0x00)
	}
}

// maxRows is the most rows one GS v 0 block's two-byte height holds.
const maxRows = 0xFFFF

func (p *phomemo) encode(job *driver.Job) ([]byte, error) {
	var b []byte
	for i, pg := range job.Pages {
		if band := p.model.Quirk("band_rows", 0); (band <= 0 || band > maxRows) && pg.Bounds().Dy() > maxRows {
			return nil, &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("page %d is %d rows long, %s takes at most %d", i+1, pg.Bounds().Dy(), p.model.ID, maxRows)}
		}
		b = p.page(b, pg, job.Media)
	}
	if p.model.Dialect == "m02" {
		b = append(b, 0x1B, 0x64, 0x02) // and once more at the end
	}
	return b, nil
}

func (p *phomemo) Encode(w io.Writer, job *driver.Job) error {
	b, err := p.encode(job)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// reader decodes `1A type value…` replies off the notification stream; BLE credit records (`01 n`,
// `02 lo hi`) and anything else are skipped.
type reader struct {
	t      driver.Transport
	buf    []byte
	seen   map[byte]byte // last value per reply type
	done   int           // 1A 0F 0C replies
	fail   byte          // a 1A 0F value other than 0C…
	failed bool          // …when one came (00 is a failure code too)
}

// replyLen is a reply's value length by type (most are one byte).
func replyLen(typ byte) int {
	switch typ {
	case 0x0D, 0x18, 0x3C:
		return 0
	case 0x07:
		return 3
	case 0x08:
		return 15
	case 0x3B:
		return 5
	}
	return 1
}

// take decodes what is buffered; a reply cut off stays buffered.
func (r *reader) take() {
	for len(r.buf) > 0 {
		switch r.buf[0] {
		case 0x1A:
			if len(r.buf) < 2 || len(r.buf) < 2+replyLen(r.buf[1]) {
				return
			}
			typ, n := r.buf[1], replyLen(r.buf[1])
			if n == 1 {
				v := r.buf[2]
				r.seen[typ] = v
				if typ == replyDone && v == doneOK {
					r.done++
				} else if typ == replyDone {
					r.fail, r.failed = v, true
				}
			}
			r.buf = r.buf[2+n:]
		case 0x01, 0x02: // 01 n, 02 lo hi
			n := int(r.buf[0]) + 1
			if len(r.buf) < n {
				return
			}
			r.buf = r.buf[n:]
		default:
			r.buf = r.buf[1:]
		}
	}
}

// read appends what one Read brings and decodes it; a timeout is no error.
func (r *reader) read() error {
	p := make([]byte, 512)
	n, err := r.t.Read(p)
	r.buf = append(r.buf, p[:n]...)
	r.take()
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, driver.ErrNoReadback) {
		return err
	}
	return nil
}

func (p *phomemo) statusOf(seen map[byte]byte) *driver.Status {
	st := &driver.Status{Code: driver.OK, Model: p.model.ID, Idle: true}
	switch {
	case seen[replyCover] == 0x99:
		st.Code, st.Detail = driver.CoverOpen, "cover open"
	case seen[replyPaper] == 0x88:
		st.Code, st.Detail = driver.NoMedia, "out of paper"
	case seen[replyTemp] == 0xA9:
		st.Code, st.Detail = driver.Overheat, "overheated"
	}
	return st
}

// Status asks for paper and cover (`1F 11 11`, `1F 11 12`) and waits for both replies; ErrNoReadback
// when neither comes.
func (p *phomemo) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	r := &reader{t: t, seen: map[byte]byte{}}
	if _, err := t.Write([]byte{0x1F, 0x11, 0x11, 0x1F, 0x11, 0x12}); err != nil {
		return nil, err
	}
	for end := time.Now().Add(replyWait); time.Now().Before(end) && ctx.Err() == nil; {
		_, paper := r.seen[replyPaper]
		_, cover := r.seen[replyCover]
		if paper && cover {
			break
		}
		if err := r.read(); err != nil {
			return nil, err
		}
	}
	_, paper := r.seen[replyPaper]
	_, cover := r.seen[replyCover]
	if !paper && !cover {
		return nil, driver.ErrNoReadback
	}
	st := p.statusOf(r.seen)
	st.Via, st.Raw = t.Kind(), fmt.Sprintf("paper %02x cover %02x", r.seen[replyPaper], r.seen[replyCover])
	return st, nil
}

// Print sends the whole job, then waits for a `1A 0F 0C` per page: disconnecting earlier loses the last
// rows. A paper, cover or heat alarm while printing is an error.
func (p *phomemo) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	if st := job.Status; st != nil && st.Code != driver.OK {
		return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
	}
	b, err := p.encode(job)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &reader{t: t, seen: map[byte]byte{}}
	if _, err := t.Write(b); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error(), Err: err}
	}
	res := &driver.Result{Pages: len(job.Pages), Bytes: len(b)}
	if job.Media != nil {
		res.Media = job.Media.ID
	}
	// alarms are checked before the page count, so an alarm in the read that brings the last page still fails
	for end := time.Now().Add(confirmBase + confirmPerPage*time.Duration(len(job.Pages))); ; {
		if st := p.statusOf(r.seen); st.Code != driver.OK {
			return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
		}
		if r.failed {
			return nil, &driver.Error{Code: driver.Unknown, Detail: fmt.Sprintf("printer reports the print failed (1A 0F %02X)", r.fail)}
		}
		switch {
		case r.done >= len(job.Pages):
			res.Confirmed, res.Message = true, "printed"
			return res, nil
		case ctx.Err() != nil:
			res.Message = "sent (not confirmed: stopped waiting)"
			return res, nil
		case !time.Now().Before(end):
			res.Message = "sent (not confirmed: the printer never reported the label printed)"
			return res, nil
		}
		if err := r.read(); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error(), Err: err}
		}
	}
}
