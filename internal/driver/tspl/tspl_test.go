package tspl

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// fake answers every <ESC>!? with the next scripted byte (the last one repeats; none = silence). Reads time out
// when nothing is waiting; stale bytes can be preloaded.
type fake struct {
	status  []byte
	in      []byte
	written bytes.Buffer
	readErr error
}

func (f *fake) Open(context.Context) error { return nil }
func (f *fake) Close() error               { return nil }
func (f *fake) Kind() string               { return "tcp" }
func (f *fake) Write(p []byte) (int, error) {
	if bytes.Equal(p, []byte{0x1B, '!', '?'}) {
		if len(f.status) > 0 {
			f.in = append(f.in, f.status[0])
			if len(f.status) > 1 {
				f.status = f.status[1:]
			}
		}
		return len(p), nil
	}
	return f.written.Write(p)
}
func (f *fake) Read(p []byte) (int, error) {
	if len(f.in) == 0 {
		if f.readErr != nil {
			return 0, f.readErr
		}
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func newDriver(t *testing.T, id string) (*tspl, *devicedb.Model) {
	t.Helper()
	m, ok := devicedb.Default().Model(id)
	if !ok {
		t.Fatalf("no %s", id)
	}
	d, err := driver.New(m, driver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	td := d.(*tspl)
	td.poll, td.notSeen = time.Millisecond, 50*time.Millisecond
	return td, m
}

// gray is a w×h white page with the given dots black.
func gray(w, h int, black ...image.Point) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for _, p := range black {
		img.Pix[p.Y*img.Stride+p.X] = 0
	}
	return img
}

func TestEncodeGolden(t *testing.T) {
	m := &devicedb.Model{ID: "T", Driver: "tspl", DPI: 203, HeadPins: 832}
	d := &tspl{model: m}
	die := &driver.Media{ID: "label-38x25", Kind: driver.DieCut, WidthMM: 38, LengthMM: 25}
	var b bytes.Buffer
	// 10×2 dots: (0,0) and (9,1) black → row 0 = 0x7F 0xFF, row 1 = 0xFF 0xBF
	if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{gray(10, 2, image.Pt(0, 0), image.Pt(9, 1))}, Media: die}); err != nil {
		t.Fatal(err)
	}
	want := "SIZE 38 mm,25 mm\r\nGAP 3 mm,0 mm\r\nDIRECTION 0,0\r\nREFERENCE 0,0\r\nCLS\r\n" +
		"BITMAP 0,0,2,2,0,\x7F\xFF\xFF\xBF\r\nPRINT 1,1\r\n"
	if b.String() != want {
		t.Errorf("got  %q\nwant %q", b.String(), want)
	}

	// continuous: as long as the page (16 rows at 8 dots/mm = 2 mm), no gap
	m.Quirks = map[string]any{"cutter": true}
	b.Reset()
	cont := &driver.Media{ID: "continuous-57", Kind: driver.Continuous, WidthMM: 57}
	if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{gray(8, 16)}, Media: cont, Cut: "per_label"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "SIZE 57 mm,2 mm\r\nGAP 0 mm,0 mm\r\nDIRECTION 0,0\r\nREFERENCE 0,0\r\nSET CUTTER 1\r\nCLS\r\nBITMAP 0,0,1,16,0,") {
		t.Errorf("continuous: %q", b.String())
	}
}

func TestEncodeCutModes(t *testing.T) {
	m := &devicedb.Model{ID: "T", Driver: "tspl", DPI: 203, HeadPins: 832, Quirks: map[string]any{"cutter": true}}
	md := &driver.Media{ID: "x", Kind: driver.DieCut, WidthMM: 50, LengthMM: 30}
	for cut, want := range map[string]string{"per_label": "SET CUTTER 1", "last": "SET CUTTER BATCH", "none": "SET CUTTER OFF"} {
		var b bytes.Buffer
		if err := (&tspl{model: m}).Encode(&b, &driver.Job{Pages: []*image.Gray{gray(8, 8)}, Media: md, Cut: cut}); err != nil || !strings.Contains(b.String(), want+"\r\n") {
			t.Errorf("cut %q: %v %q", cut, err, b.String())
		}
	}
	var b bytes.Buffer
	for _, d := range []*tspl{{model: m}, {model: &devicedb.Model{DPI: 203, HeadPins: 832}}} {
		cut := map[bool]string{true: "", false: "per_label"}[d.model.QuirkBool("cutter")]
		b.Reset()
		if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{gray(8, 8)}, Media: md, Cut: cut}); err != nil || strings.Contains(b.String(), "CUTTER") {
			t.Errorf("cut %q, cutter %v: %v %q", cut, d.model.QuirkBool("cutter"), err, b.String())
		}
	}
	if err := (&tspl{model: m}).Encode(&b, &driver.Job{Pages: []*image.Gray{gray(8, 8)}, Media: md, Cut: "half"}); err == nil {
		t.Error("cut half accepted")
	}
}

// A page wider than the head is cropped to it.
func TestEncodeHeadClamp(t *testing.T) {
	d := &tspl{model: &devicedb.Model{DPI: 203, HeadPins: 12}}
	var b bytes.Buffer
	pg := gray(20, 1, image.Pt(11, 0), image.Pt(15, 0))
	if err := d.Encode(&b, &driver.Job{Pages: []*image.Gray{pg}, Media: &driver.Media{Kind: driver.DieCut, WidthMM: 2, LengthMM: 1}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "BITMAP 0,0,2,1,0,\xFF\xEF\r\n") {
		t.Errorf("%q", b.String())
	}
}

func TestStatusParsing(t *testing.T) {
	d, _ := newDriver(t, "TE200")
	for s, want := range map[byte]driver.Code{
		0x00: driver.OK, 0x20: driver.OK, 0x01: driver.CoverOpen, 0x40: driver.Unknown, 0x02: driver.PaperJam,
		0x04: driver.NoMedia, 0x08: driver.RibbonOut, 0x10: driver.Paused, 0x80: driver.Unknown, 0x05: driver.CoverOpen,
	} {
		st, err := d.Status(context.Background(), &fake{status: []byte{s}})
		if err != nil || st.Code != want || st.Idle != (s&printing == 0) || (want != driver.OK && st.Detail == "") {
			t.Errorf("%02x: %+v %v, want %s", s, st, err, want)
		}
	}
}

// Stale bytes from an earlier, late answer are dropped before asking.
func TestStatusDrainsStale(t *testing.T) {
	d, _ := newDriver(t, "TE200")
	st, err := d.Status(context.Background(), &fake{in: []byte{0x04, 0x04}, status: []byte{0x00}})
	if err != nil || st.Code != driver.OK {
		t.Errorf("%+v %v", st, err)
	}
}

func TestStatusNoAnswer(t *testing.T) {
	d, _ := newDriver(t, "TE200")
	if _, err := d.Status(context.Background(), &fake{}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silent: %v", err)
	}
	if _, err := d.Status(context.Background(), &fake{readErr: driver.ErrNoReadback}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("write-only transport: %v", err)
	}
	broken := errors.New("broken pipe")
	if _, err := d.Status(context.Background(), &fake{readErr: broken}); !errors.Is(err, broken) {
		t.Errorf("broken: %v", err)
	}
}

func job(m *devicedb.Model, st *driver.Status) *driver.Job {
	md := devicedb.Default().MediaFor(m)[0]
	return &driver.Job{Pages: []*image.Gray{gray(md.PrintableWidthDots, md.PrintableLengthDots)}, Media: &md, Status: st}
}

func TestPrint(t *testing.T) {
	d, m := newDriver(t, "TE200")
	ok := &driver.Status{Code: driver.OK}
	for name, tc := range map[string]struct {
		status    []byte
		st        *driver.Status
		confirmed bool
		msg       string
		code      driver.Code
	}{
		"confirmed":     {[]byte{0x20, 0x20, 0x00}, ok, true, "printed", ""},
		"never seen":    {[]byte{0x00}, ok, false, "probably printed", ""},
		"no status":     {nil, nil, false, "doesn't report status", ""},
		"paper out":     {[]byte{0x20, 0x04}, ok, false, "", driver.NoMedia},
		"refused first": {nil, &driver.Status{Code: driver.CoverOpen, Detail: "cover open"}, false, "", driver.CoverOpen},
	} {
		f := &fake{status: tc.status}
		res, err := d.Print(context.Background(), f, job(m, tc.st))
		if tc.code != "" {
			var de *driver.Error
			if !errors.As(err, &de) || de.Code != tc.code {
				t.Errorf("%s: %+v %v, want %s", name, res, err, tc.code)
			}
			continue
		}
		if err != nil || res.Confirmed != tc.confirmed || !strings.Contains(res.Message, tc.msg) || !strings.HasPrefix(f.written.String(), "SIZE ") {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
}

func TestModels(t *testing.T) {
	db := devicedb.Default()
	ms := db.Models("tspl")
	if len(ms) == 0 {
		t.Fatal("no tspl models")
	}
	for _, m := range ms {
		if m.HeadPins <= 0 || (m.DPI != 203 && m.DPI != 300) || len(m.Transports) == 0 || len(m.Media) == 0 {
			t.Errorf("%s: %+v", m.ID, m)
		}
		for _, md := range db.MediaFor(m) {
			if md.PrintableWidthDots > m.HeadPins || md.DPI != m.DPI {
				t.Errorf("%s: media %s %+v", m.ID, md.ID, md)
			}
		}
		if _, err := driver.New(m, driver.Options{}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
	}
}
