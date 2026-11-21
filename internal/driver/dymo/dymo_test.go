package dymo

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"image"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fake is a USB LabelWriter: each ESC A written queues the next scripted status byte (nil = silence).
type fake struct {
	replies [][]byte
	in      []byte
	written bytes.Buffer
	asked   int
}

func (f *fake) Open(context.Context) error { return nil }
func (f *fake) Close() error               { return nil }
func (f *fake) Kind() string               { return "usb" }
func (f *fake) Write(p []byte) (int, error) {
	f.written.Write(p)
	if bytes.Equal(p, statusRequest) {
		f.asked++
		if len(f.replies) > 0 {
			f.in = append(f.in, f.replies[0]...)
			f.replies = f.replies[1:]
		}
	}
	return len(p), nil
}
func (f *fake) Read(p []byte) (int, error) {
	if len(f.in) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func newDriver(t *testing.T, m *devicedb.Model, o driver.Options) *dymo {
	t.Helper()
	d, err := driver.New(m, o)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*dymo)
}

func dbModel(t *testing.T, id string) *devicedb.Model {
	t.Helper()
	m, ok := devicedb.Default().Model(id)
	if !ok {
		t.Fatalf("no model %s", id)
	}
	return m
}

func dbMedia(t *testing.T, m *devicedb.Model, id string) *devicedb.Media {
	t.Helper()
	for _, md := range devicedb.Default().MediaFor(m) {
		if md.ID == id {
			return &md
		}
	}
	t.Fatalf("%s takes no %s", m.ID, id)
	return nil
}

// page is w×h white with the given dots black.
func page(w, h int, black ...image.Point) *image.Gray {
	p := image.NewGray(image.Rect(0, 0, w, h))
	for i := range p.Pix {
		p.Pix[i] = 0xff
	}
	for _, b := range black {
		p.Pix[b.Y*p.Stride+b.X] = 0
	}
	return p
}

// tiny is a 16-dot head, small enough to write its byte stream out by hand from the tech ref.
func tiny(dialect string) *devicedb.Model {
	return &devicedb.Model{ID: "Tiny", Driver: "dymo", Dialect: dialect, DPI: 300, HeadPins: 16}
}

var label = &devicedb.Media{ID: "t-12x3", Kind: devicedb.DieCut, PrintableWidthDots: 12, PrintableLengthDots: 3, DPI: 300}

func TestEncodeBytes(t *testing.T) {
	pages := []*image.Gray{page(12, 2, image.Pt(0, 0)), page(12, 1, image.Pt(11, 0))}
	head := []byte{esc, esc, esc, esc, 'B', 0, esc, 'D', 2, esc, 'L', 0, 63} // 3 lines + 60 slack
	lines := []byte{
		syn, 0x80, 0x00, // 1 black + 15 white: RLE is 2 bytes, not shorter
		etb, 0x0f, // 16 white
		esc, 'G',
		syn, 0x00, 0x10, // dot 11: RLE 0a 80 03 is longer
		esc, 'E',
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	for name, tc := range map[string]struct {
		model *devicedb.Model
		opts  driver.Options
		media *devicedb.Media
		want  []byte
	}{
		"lw400": {tiny("lw400"), driver.Options{}, label, cat(head, []byte{esc, 'h', esc, 'e'}, lines)},
		"dark":  {tiny("lw400"), driver.Options{Darkness: "dark"}, label, cat(head, []byte{esc, 'h', esc, 'g'}, lines)},
		"light": {tiny("lw400"), driver.Options{Darkness: "light"}, label, cat(head, []byte{esc, 'h', esc, 'c'}, lines)},
		"lw300": {tiny("lw300"), driver.Options{}, label, cat(head,
			[]byte{syn, 0x80, 0x00, syn, 0x00, 0x00, esc, 'G', syn, 0x00, 0x10, esc, 'E'})},
		"continuous": {tiny("lw400"), driver.Options{}, &devicedb.Media{ID: "t-12", Kind: devicedb.Continuous, PrintableWidthDots: 12},
			cat(head[:11], []byte{0xff, 0xff, esc, 'h', esc, 'e'}, lines)},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			if err := newDriver(t, tc.model, tc.opts).Encode(&b, &driver.Job{Pages: pages, Media: tc.media}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b.Bytes(), tc.want) {
				t.Errorf("got  % x\nwant % x", b.Bytes(), tc.want)
			}
		})
	}
}

// Golden stream for a real model and label; a change here moves every label printed.
func TestEncodeGolden(t *testing.T) {
	for _, tc := range []struct{ model, media string }{{"LabelWriter-450", "dymo-99012"}, {"LabelWriter-4XL", "dymo-1744907"}, {"LabelWriter-330", "dymo-11354"}} {
		t.Run(tc.model, func(t *testing.T) {
			m := dbModel(t, tc.model)
			md := dbMedia(t, m, tc.media)
			w, h := md.PrintableWidthDots, md.PrintableLengthDots
			var dots []image.Point
			for i := range min(w, h) { // a diagonal, a border and a bar: runs, lone dots, full lines
				dots = append(dots, image.Pt(i, i), image.Pt(0, i), image.Pt(w-1, i), image.Pt(i, 0))
			}
			for y := h / 2; y < h/2+20; y++ {
				for x := range w {
					dots = append(dots, image.Pt(x, y))
				}
			}
			var b bytes.Buffer
			if err := newDriver(t, m, driver.Options{}).Encode(&b, &driver.Job{Pages: []*image.Gray{page(w, h, dots...), page(w, h)}, Media: md}); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", strings.ToLower(tc.model)+"_"+tc.media+".bin")
			if *update {
				if err := os.WriteFile(golden, b.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b.Bytes(), want) {
				t.Errorf("output differs from %s (run with -update after checking the change)", golden)
			}
		})
	}
}

// decompress inverts compress: the runs must cover exactly n dots.
func decompress(t *testing.T, c []byte, n int) []byte {
	t.Helper()
	out := make([]byte, n/8)
	i := 0
	for _, r := range c {
		for range int(r&0x7f) + 1 {
			if i >= n {
				t.Fatalf("runs exceed %d dots", n)
			}
			if r&0x80 != 0 {
				out[i/8] |= 0x80 >> (i % 8)
			}
			i++
		}
	}
	if i != n {
		t.Fatalf("runs cover %d dots, want %d", i, n)
	}
	return out
}

func TestCompress(t *testing.T) {
	full := bytes.Repeat([]byte{0xff}, 84)
	if got, want := compress(full), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x9f}; !bytes.Equal(got, want) { // 5×128 + 32
		t.Errorf("black line: % x, want % x", got, want)
	}
	if got := compress(make([]byte, 2)); !bytes.Equal(got, []byte{0x0f}) { // the tech ref's "0Fh = 16 white pixels"
		t.Errorf("white: % x", got)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		line := make([]byte, 1+r.IntN(156))
		for i := range line {
			line[i] = []byte{0, 0xff, byte(r.Uint32())}[r.IntN(3)]
		}
		if got := decompress(t, compress(line), len(line)*8); !bytes.Equal(got, line) {
			t.Fatalf("round trip % x → % x", line, got)
		}
	}
}

func TestParseStatus(t *testing.T) {
	for _, tc := range []struct {
		b    byte
		code driver.Code
		idle bool
	}{
		{0x03, driver.OK, true}, // ready + top of form ("printer ready is returned as 03h")
		{0x01, driver.OK, false},
		{0x00, driver.OK, false}, // bit 0 ignored (inverted in the SE450 reference)
		{0xa1, driver.NoMedia, false},
		{0x23, driver.NoMedia, false},
		{0xc1, driver.PaperJam, false},
		{0x81, driver.Unknown, false},
		{0x1f, driver.OK, true}, // reserved bits ignored
	} {
		s := parseStatus(tc.b)
		if s.Code != tc.code || s.Idle != tc.idle || s.Raw == "" {
			t.Errorf("%02x: %+v, want %s idle=%v", tc.b, s, tc.code, tc.idle)
		}
	}
}

func TestStatus(t *testing.T) {
	d := newDriver(t, dbModel(t, "LabelWriter-450"), driver.Options{})
	f := &fake{replies: [][]byte{{0x03}}}
	s, err := d.Status(context.Background(), f)
	if err != nil || s.Code != driver.OK || s.Via != "usb" || s.Model != "LabelWriter-450" {
		t.Fatalf("%+v %v", s, err)
	}
	if !bytes.Equal(f.written.Bytes(), []byte{esc, 'A'}) {
		t.Errorf("wrote % x", f.written.Bytes())
	}
	// no answer (unidirectional interface, or a printer that doesn't know ESC A): print blind
	if _, err := d.Status(context.Background(), &fake{}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silence: %v, want ErrNoReadback", err)
	}
}

func TestPrint(t *testing.T) {
	m := dbModel(t, "LabelWriter-450")
	d := newDriver(t, m, driver.Options{})
	md := dbMedia(t, m, "dymo-11354")
	pages := []*image.Gray{page(100, 100, image.Pt(5, 5))}
	answered := &driver.Status{Code: driver.OK, Via: "usb"}
	for name, tc := range map[string]struct {
		status  *driver.Status
		replies [][]byte
		code    driver.Code
		msg     string
		asked   int
	}{
		"blind":       {nil, nil, "", "no status", 0},
		"ok after":    {answered, [][]byte{{0x03}}, "", "no error", 1},
		"silent":      {answered, nil, "", "no status after sending", 1},
		"empty after": {answered, [][]byte{{0xa1}}, driver.NoMedia, "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{replies: tc.replies}
			res, err := d.Print(context.Background(), f, &driver.Job{Pages: pages, Media: md, Status: tc.status})
			if f.asked != tc.asked {
				t.Errorf("asked for status %d times, want %d", f.asked, tc.asked)
			}
			if tc.code != "" {
				var de *driver.Error
				if !errors.As(err, &de) || de.Code != tc.code {
					t.Fatalf("err %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || res.Confirmed || !strings.Contains(res.Message, tc.msg) || res.Bytes == 0 {
				t.Errorf("%+v %v", res, err)
			}
		})
	}
}

// The page may be no wider than the media's printable width, nor than the head (here a media wider
// than the 16-dot head): ESC D never asks for more bytes than the head has.
func TestHeadClamp(t *testing.T) {
	d := newDriver(t, tiny("lw400"), driver.Options{})
	wide := &devicedb.Media{ID: "t-wide", Kind: devicedb.Continuous, PrintableWidthDots: 24}
	var b bytes.Buffer
	if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{page(16, 1)}, Media: wide}); err != nil {
		t.Fatal(err)
	}
	if got := b.Bytes()[6:9]; !bytes.Equal(got, []byte{esc, 'D', 2}) {
		t.Errorf("bytes per line % x, want 1b 44 02", got)
	}
	for name, job := range map[string]*driver.Job{
		"wider than head":   {Pages: []*image.Gray{page(17, 1)}, Media: wide},
		"wider than media":  {Pages: []*image.Gray{page(13, 1)}, Media: label},
		"longer than label": {Pages: []*image.Gray{page(12, 4)}, Media: label},
	} {
		var de *driver.Error
		if err := d.Encode(&b, job); !errors.As(err, &de) || de.Code != driver.MediaMismatch {
			t.Errorf("%s: %v, want MEDIA_MISMATCH", name, err)
		}
	}
	var de *driver.Error
	if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{page(12, 1)}, Media: label, Cut: "per_label"}); !errors.As(err, &de) || de.Code != driver.Unsupported {
		t.Errorf("cut per_label: %v, want UNSUPPORTED (no cutter)", err)
	}
}

func TestNewRejects(t *testing.T) {
	for _, m := range []*devicedb.Model{
		{ID: "X", Driver: "dymo", Dialect: "lw500", HeadPins: 672},
		{ID: "Y", Driver: "dymo", Dialect: "lw400", HeadPins: 670},
	} {
		if _, err := driver.New(m, driver.Options{}); err == nil {
			t.Errorf("%s accepted", m.ID)
		}
	}
	for _, m := range devicedb.Default().Models("dymo") {
		if _, err := driver.New(m, driver.Options{}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
	}
}
