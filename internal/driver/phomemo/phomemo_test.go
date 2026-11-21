package phomemo

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// page is a w×h white page with black dots where black(x, y).
func page(w, h int, black func(x, y int) bool) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.Gray{Y: 255}
			if black(x, y) {
				c.Y = 0
			}
			img.SetGray(x, y, c)
		}
	}
	return img
}

func newDriver(t *testing.T, id, darkness string) *phomemo {
	t.Helper()
	m, ok := devicedb.Default().Model(id)
	if !ok {
		t.Fatalf("no %s", id)
	}
	d, err := driver.New(m, driver.Options{Darkness: darkness})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*phomemo)
}

func TestRaster(t *testing.T) {
	img := page(4, 3, func(x, y int) bool { return x == 0 })
	// one block, centred on a 16-dot head: 6 white dots, then the 4 dots
	got := raster(nil, img, 16, 0, "center")
	want := []byte{0x1D, 0x76, 0x30, 0x00, 2, 0, 3, 0, 0x02, 0x00, 0x02, 0x00, 0x02, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("centre: % X", got)
	}
	// blocks of 2 rows, right-aligned
	got = raster(nil, img, 16, 2, "right")
	want = []byte{0x1D, 0x76, 0x30, 0x00, 2, 0, 2, 0, 0x00, 0x08, 0x00, 0x08, 0x1D, 0x76, 0x30, 0x00, 2, 0, 1, 0, 0x00, 0x08}
	if !bytes.Equal(got, want) {
		t.Errorf("right: % X", got)
	}
	// dots past the head are cut off
	got = raster(nil, page(20, 1, func(x, _ int) bool { return x >= 16 }), 16, 0, "")
	if !bytes.Equal(got[8:], []byte{0, 0}) {
		t.Errorf("clamp: % X", got)
	}
}

// Golden jobs per dialect (a 1×1 black page).
func TestEncode(t *testing.T) {
	gap := &devicedb.Media{ID: "phomemo-40x30", Kind: devicedb.DieCut}
	cont := &devicedb.Media{ID: "phomemo-40", Kind: devicedb.Continuous}
	px := []*image.Gray{page(1, 1, func(int, int) bool { return true })}
	row := func(bytes int, first byte) []byte { r := make([]byte, bytes); r[0] = first; return r }
	cat := func(bs ...[]byte) []byte { return bytes.Join(bs, nil) }
	for _, tc := range []struct {
		id, dark string
		media    *devicedb.Media
		want     []byte
	}{
		{"M02", "", nil, cat([]byte{0x1B, 0x40, 0x1F, 0x11, 0x02, 0x04, 0x1D, 0x76, 0x30, 0x00, 48, 0, 1, 0},
			row(48, 0x01), []byte{0x1B, 0x64, 0x02, 0x1B, 0x64, 0x02})}, // a 1-dot page centred on 384 dots: dot 191
		{"M110", "dark", gap, cat([]byte{0x1B, 0x4E, 0x0D, 3, 0x1B, 0x4E, 0x04, 15, 0x1F, 0x11, 0x0A, 0x1D, 0x76, 0x30, 0x00, 48, 0, 1, 0},
			row(48, 0x01), []byte{0x1F, 0xF0, 0x05, 0x00, 0x1F, 0xF0, 0x03, 0x00})},
		{"M110", "", cont, cat([]byte{0x1B, 0x4E, 0x0D, 3, 0x1B, 0x4E, 0x04, 10, 0x1F, 0x11, 0x0B, 0x1D, 0x76, 0x30, 0x00, 48, 0, 1, 0},
			row(48, 0x01), []byte{0x1F, 0xF0, 0x05, 0x00, 0x1F, 0xF0, 0x03, 0x00})},
		{"D30", "", gap, cat([]byte{0x1F, 0x11, 0x24, 0x00, 0x1B, 0x40, 0x1D, 0x76, 0x30, 0x00, 12, 0, 1, 0},
			make([]byte, 11), []byte{0x01, 0x1B, 0x64, 0x00})}, // right-aligned: the last dot
	} {
		var b bytes.Buffer
		if err := newDriver(t, tc.id, tc.dark).Encode(&b, &driver.Job{Pages: px, Media: tc.media}); err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if tc.id != "D30" { // centred: dot 191 is the last of byte 23
			want = bytes.Replace(want, row(48, 0x01), append(append(make([]byte, 23), 0x01), make([]byte, 24)...), 1)
		}
		if !bytes.Equal(b.Bytes(), want) {
			t.Errorf("%s %s:\n got % X\nwant % X", tc.id, tc.dark, b.Bytes(), want)
		}
	}
}

func TestReader(t *testing.T) {
	r := &reader{seen: map[byte]byte{}}
	// credits, a battery reply, a cut-off firmware reply, then the rest of it and a done
	r.buf = []byte{0x01, 0x01, 0x1A, 0x04, 0x50, 0x02, 0xB6, 0x00, 0x1A, 0x07, 0x02}
	r.take()
	if r.seen[replyBattery] != 0x50 || len(r.buf) != 3 {
		t.Fatalf("seen %v buf % X", r.seen, r.buf)
	}
	r.buf = append(r.buf, 0x00, 0x06, 0x1A, 0x0F, 0x0C, 0xFF, 0x1A, 0x08)
	r.take()
	if r.done != 1 || !bytes.Equal(r.buf, []byte{0x1A, 0x08}) {
		t.Errorf("done %d buf % X", r.done, r.buf)
	}
}

// printer is a scripted Phomemo: status queries get the replies in status, a job gets onJob.
type printer struct {
	mu      sync.Mutex
	in      []byte
	written bytes.Buffer
	status  []byte
	onJob   []byte
}

func (f *printer) Open(context.Context) error { return nil }
func (f *printer) Close() error               { return nil }
func (f *printer) Kind() string               { return "ble" }

func (f *printer) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written.Write(p)
	if bytes.HasPrefix(p, []byte{0x1F, 0x11, 0x11}) {
		f.in = append(f.in, f.status...)
	} else {
		f.in = append(f.in, f.onJob...)
	}
	return len(p), nil
}

func (f *printer) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		f.mu.Unlock()
		time.Sleep(time.Millisecond)
		f.mu.Lock()
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in[:min(len(f.in), 3)]) // notifications come in pieces
	f.in = f.in[n:]
	return n, nil
}

func fast(t *testing.T) {
	vars := []*time.Duration{&replyWait, &confirmBase, &confirmPerPage}
	vals := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 0}
	for i, v := range vars {
		old := *v
		*v = vals[i]
		t.Cleanup(func() { *v = old })
	}
}

func TestStatus(t *testing.T) {
	fast(t)
	for _, tc := range []struct {
		reply []byte
		code  driver.Code
	}{
		{[]byte{0x01, 0x01, 0x1A, 0x06, 0x89, 0x1A, 0x05, 0x98}, driver.OK},
		{[]byte{0x1A, 0x06, 0x88, 0x1A, 0x05, 0x98}, driver.NoMedia},
		{[]byte{0x1A, 0x06, 0x89, 0x1A, 0x05, 0x99}, driver.CoverOpen},
		{[]byte{0x1A, 0x06, 0x89}, driver.OK}, // the cover reply never came
	} {
		st, err := newDriver(t, "M110", "").Status(context.Background(), &printer{status: tc.reply})
		if err != nil || st.Code != tc.code || st.Model != "M110" {
			t.Errorf("% X: %+v %v", tc.reply, st, err)
		}
	}
	if _, err := newDriver(t, "M110", "").Status(context.Background(), &printer{}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silent: %v", err)
	}
}

func TestPrint(t *testing.T) {
	fast(t)
	pages := []*image.Gray{page(8, 8, func(x, y int) bool { return x == y }), page(8, 8, func(int, int) bool { return false })}
	job := func() *driver.Job {
		return &driver.Job{Pages: pages, Media: &devicedb.Media{ID: "phomemo-40x30"}, Status: &driver.Status{Code: driver.OK}}
	}
	d := newDriver(t, "M110", "")
	for _, tc := range []struct {
		name      string
		onJob     []byte
		confirmed bool
		code      driver.Code
	}{
		{"two pages done", []byte{0x01, 0x01, 0x1A, 0x0F, 0x0C, 0x01, 0x01, 0x1A, 0x0F, 0x0C}, true, ""},
		{"one page done", []byte{0x1A, 0x0F, 0x0C}, false, ""},
		{"paper out", []byte{0x1A, 0x0F, 0x0C, 0x1A, 0x06, 0x88}, false, driver.NoMedia},
		{"failed", []byte{0x1A, 0x0F, 0x01}, false, driver.Unknown},
		{"failed with code 00", []byte{0x1A, 0x0F, 0x00}, false, driver.Unknown},
		{"paper out with the last page", []byte{0x1A, 0x0F, 0x0C, 0x1A, 0x06, 0x88, 0x1A, 0x0F, 0x0C}, false, driver.NoMedia},
	} {
		f := &printer{onJob: tc.onJob}
		res, err := d.Print(context.Background(), f, job())
		var de *driver.Error
		switch {
		case tc.code != "":
			if !errors.As(err, &de) || de.Code != tc.code {
				t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
			}
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case res.Confirmed != tc.confirmed || res.Pages != 2 || res.Bytes != f.written.Len():
			t.Errorf("%s: %+v", tc.name, res)
		case !tc.confirmed && !strings.Contains(res.Message, "not confirmed"):
			t.Errorf("%s: %q", tc.name, res.Message)
		}
	}
	// a bad status before the job sends nothing
	f := &printer{}
	j := job()
	j.Status = &driver.Status{Code: driver.CoverOpen}
	if _, err := d.Print(context.Background(), f, j); err == nil || f.written.Len() != 0 {
		t.Errorf("%v, %d bytes", err, f.written.Len())
	}
	// a page too long for one block's two-byte height sends nothing
	f = &printer{}
	j = job()
	j.Pages = []*image.Gray{image.NewGray(image.Rect(0, 0, 8, maxRows+1))}
	if _, err := d.Print(context.Background(), f, j); err == nil || f.written.Len() != 0 {
		t.Errorf("long page: %v, %d bytes", err, f.written.Len())
	}
}

func TestModels(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct{ name, id string }{
		{"M02", "M02"}, {"Mr.in_M02", "M02"}, {"M02S", "M02S"}, {"M02 Pro", "M02-PRO"}, {"T02", "T02"},
		{"M110", "M110"}, {"M110S", "M110"}, {"M120", "M120"}, {"M221", "M221"}, {"Q30", "Q30"}, {"Q30S", "Q30"},
		{"D30", "D30"}, {"D35", "D35"},
	} {
		m, ok := db.ModelByBLEName(tc.name)
		if !ok || m.ID != tc.id {
			t.Errorf("%s: %v %v, want %s", tc.name, m, ok, tc.id)
		}
	}
	if m, ok := db.ModelByBLEName("Q306E12345678"); ok { // an M120 advertising its serial
		t.Errorf("Q306…: %s", m.ID)
	}
	for _, m := range db.Models("phomemo") {
		if _, err := driver.New(m, driver.Options{}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
		if m.Quirk("ble_service", 0) != 0xff00 || m.Quirk("ble_write", 0) != 0xff02 || m.Quirk("ble_notify", 0) != 0xff03 {
			t.Errorf("%s: %v", m.ID, m.Quirks)
		}
		if m.QuirkBool("darkness") != (m.Dialect == "m110") {
			t.Errorf("%s: darkness quirk on dialect %s", m.ID, m.Dialect)
		}
		for _, md := range db.MediaFor(m) {
			if md.PrintableWidthDots > m.HeadPins || md.PrintableWidthDots == 0 {
				t.Errorf("%s %s: %d dots", m.ID, md.ID, md.PrintableWidthDots)
			}
		}
	}
}
