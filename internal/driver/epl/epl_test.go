package epl_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
	"github.com/francisrath/pdb-print/internal/driver/epl"
)

// fake is an EPL printer behind a transport of the given kind: it records what it gets and answers
// each ^ee with the next of replies (the last one repeats); no replies = silent.
type fake struct {
	kind     string
	mu       sync.Mutex
	replies  []string
	sent     bytes.Buffer
	pending  []byte
	closeErr error
}

func (f *fake) Open(context.Context) error { return nil }
func (f *fake) Close() error               { return f.closeErr }
func (f *fake) Kind() string               { return f.kind }

func (f *fake) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(p) == "^ee\n" {
		if len(f.replies) > 0 {
			f.pending = []byte(f.replies[0] + "\r\n")
			if len(f.replies) > 1 {
				f.replies = f.replies[1:]
			}
		}
		return len(p), nil
	}
	return f.sent.Write(p)
}

func (f *fake) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func newDriver(t *testing.T, id string, o driver.Options) driver.Driver {
	t.Helper()
	m, ok := devicedb.Default().Model(id)
	if !ok {
		t.Fatalf("no model %s", id)
	}
	d, err := driver.New(m, o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// white returns a w×h white page with the given dots black.
func white(w, h int, black ...image.Point) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for _, p := range black {
		img.Pix[p.Y*img.Stride+p.X] = 0
	}
	return img
}

func dieCut(w, l int) *devicedb.Media {
	return &devicedb.Media{ID: "test", Kind: devicedb.DieCut, PrintableWidthDots: w, PrintableLengthDots: l, DPI: 203}
}

func encode(t *testing.T, d driver.Driver, job *driver.Job) string {
	t.Helper()
	var b bytes.Buffer
	if err := d.Encode(&b, job); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Golden bytes, and EPL's GW polarity: a 0 bit is a black dot, most significant bit leftmost.
func TestEncodeGolden(t *testing.T) {
	d := newDriver(t, "LP2844", driver.Options{})
	pg := white(16, 2, image.Pt(0, 0), image.Pt(15, 1))
	got := encode(t, d, &driver.Job{Pages: []*image.Gray{pg, pg}, Media: dieCut(16, 2)})
	page := "N\nGW0,0,2,2,\x7f\xff\xff\xfe\nP1\n"
	if want := "\nZT\nq16\nQ2,24\nD10\n" + page + page; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Smaller pages are padded white, larger ones clipped to the label.
func TestEncodePadsAndClips(t *testing.T) {
	d := newDriver(t, "LP2844", driver.Options{})
	got := encode(t, d, &driver.Job{Pages: []*image.Gray{white(4, 1, image.Pt(3, 0))}, Media: dieCut(10, 2)})
	if !strings.HasSuffix(got, "GW0,0,2,2,\xef\xff\xff\xff\nP1\n") {
		t.Errorf("padded: %q", got)
	}
	got = encode(t, d, &driver.Job{Pages: []*image.Gray{white(40, 9, image.Pt(39, 8))}, Media: dieCut(8, 1)})
	if !strings.HasSuffix(got, "q8\nQ1,24\nD10\nN\nGW0,0,1,1,\xff\nP1\n") {
		t.Errorf("clipped: %q", got)
	}
}

func TestEncodeForms(t *testing.T) {
	pg := white(8, 30)
	for _, tc := range []struct {
		name  string
		model string
		o     driver.Options
		media *devicedb.Media
		want  string
	}{
		{"gap 203 dpi", "LP2844", driver.Options{}, dieCut(8, 30), "\nQ30,24\n"},
		{"gap 300 dpi", "TLP3842", driver.Options{}, dieCut(8, 30), "\nQ30,36\n"},
		{"black mark", "LP2844", driver.Options{BlackMarkMM: 4}, dieCut(8, 30), "\nQ30,B32+0\n"},
		{"black mark below the minimum", "LP2844", driver.Options{BlackMarkMM: 0.5}, dieCut(8, 30), "\nQ30,B16+0\n"},
		{"black mark below the 300 dpi minimum", "TLP3842", driver.Options{BlackMarkMM: 0.5}, dieCut(8, 30), "\nQ30,B18+0\n"},
		{"continuous: page length", "LP2844", driver.Options{}, &devicedb.Media{ID: "roll", Kind: devicedb.Continuous, PrintableWidthDots: 8}, "\nQ30,0\n"},
		{"continuous ignores black mark", "LP2844", driver.Options{BlackMarkMM: 4}, &devicedb.Media{ID: "roll", Kind: devicedb.Continuous, PrintableWidthDots: 8}, "\nQ30,0\n"},
		{"light", "LP2844", driver.Options{Darkness: "light"}, dieCut(8, 30), "\nD7\n"},
		{"dark", "LP2844", driver.Options{Darkness: "dark"}, dieCut(8, 30), "\nD13\n"},
		{"default density", "LP2824", driver.Options{}, dieCut(8, 30), "\nD7\n"},
	} {
		got := encode(t, newDriver(t, tc.model, tc.o), &driver.Job{Pages: []*image.Gray{pg}, Media: tc.media})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// q never exceeds the head, even for media wider than it.
func TestEncodeClampsToHead(t *testing.T) {
	got := encode(t, newDriver(t, "LP2824", driver.Options{}), &driver.Job{Pages: []*image.Gray{white(900, 1)}, Media: dieCut(900, 1)})
	if !strings.Contains(got, "\nq448\n") || !strings.Contains(got, "GW0,0,56,1,") {
		t.Errorf("%.60q", got)
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		reply  string
		code   driver.Code
		idle   bool
		detail bool
	}{
		{"00", driver.OK, true, false},
		{"50", driver.OK, false, false},
		{"07", driver.NoMedia, true, true},
		{"11", driver.CoverOpen, true, true},
		{"12", driver.Paused, true, true},
		{"81", driver.CutterJam, true, true},
		{"83", driver.Unknown, true, true}, // needs feed or reset: blocks like the others
		{"84", driver.MediaMismatch, true, true},
		{"02", driver.OK, true, true}, // an earlier job's error doesn't stop the printer
	} {
		st, err := newDriver(t, "LP2844", driver.Options{}).Status(context.Background(), &fake{kind: "usb", replies: []string{tc.reply}})
		if err != nil || st.Code != tc.code || st.Idle != tc.idle || (st.Detail != "") != tc.detail || st.Via != "usb" {
			t.Errorf("%s: %+v %v", tc.reply, st, err)
		}
	}
}

// ^ee answers on USB (and serial) only; silence or garbage is no read-back, not an error state.
func TestStatusNoReadback(t *testing.T) {
	for name, tr := range map[string]*fake{
		"tcp":     {kind: "tcp", replies: []string{"00"}},
		"silent":  {kind: "usb"},
		"garbage": {kind: "usb", replies: []string{"hello"}},
	} {
		_, err := newDriver(t, "LP2844", driver.Options{}).Status(context.Background(), tr)
		if !errors.Is(err, driver.ErrNoReadback) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPrintConfirmsOverUSBAndSerial(t *testing.T) {
	defer epl.SetTiming(5*time.Millisecond, 200*time.Millisecond)()
	for _, kind := range []string{"usb", "serial"} {
		tr := &fake{kind: kind, replies: []string{"50", "50", "00"}}
		res, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{white(8, 2), white(8, 2)}, Media: dieCut(8, 2)})
		if err != nil || !res.Confirmed || res.Pages != 2 || res.Bytes != tr.sent.Len() || strings.Count(tr.sent.String(), "P1\n") != 2 {
			t.Errorf("%s: %+v %v", kind, res, err)
		}
		st, err := newDriver(t, "LP2844", driver.Options{}).Status(context.Background(), &fake{kind: kind, replies: []string{"00"}})
		if err != nil || st.Via != kind {
			t.Errorf("%s status: %+v %v", kind, st, err)
		}
	}
}

func TestPrintNeverSeenBusy(t *testing.T) {
	defer epl.SetTiming(5*time.Millisecond, 50*time.Millisecond)()
	res, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), &fake{kind: "usb", replies: []string{"00"}},
		&driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2)})
	if err != nil || res.Confirmed || !strings.Contains(res.Message, "probably printed") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestPrintOverTCPIsBlind(t *testing.T) {
	tr := &fake{kind: "tcp"}
	res, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2)})
	if err != nil || res.Confirmed || !strings.Contains(res.Message, "not confirmed") || tr.sent.Len() == 0 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestPrintErrors(t *testing.T) {
	defer epl.SetTiming(5*time.Millisecond, time.Second)()
	for _, tc := range []struct {
		name    string
		before  string // the status read before printing
		replies []string
		want    driver.Code
	}{
		{"out of labels while printing", "00", []string{"50", "07"}, driver.NoMedia},
		{"our job's error", "00", []string{"02"}, driver.Unknown},
		{"old error cleared, then ours", "02", []string{"00", "50", "02"}, driver.Unknown},
		{"illegal interrupt", "00", []string{"50", "83"}, driver.Unknown},
	} {
		job := &driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2), Status: &driver.Status{Code: driver.OK, Raw: tc.before}}
		_, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), &fake{kind: "usb", replies: tc.replies}, job)
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != tc.want {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// the same error as before the job is an old one: printed
	job := &driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2), Status: &driver.Status{Code: driver.OK, Raw: "02"}}
	res, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), &fake{kind: "usb", replies: []string{"50", "02"}}, job)
	if err != nil || !res.Confirmed {
		t.Errorf("old error: %+v %v", res, err)
	}
}

// A lost tail on close (USB drain) fails the job instead of watching ^ee.
func TestPrintCloseError(t *testing.T) {
	_, err := newDriver(t, "LP2844", driver.Options{}).Print(context.Background(), &fake{kind: "usb", replies: []string{"00"}, closeErr: errors.New("drain: unplugged")},
		&driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2)})
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError {
		t.Errorf("%v", err)
	}
}

func TestPrintStopsBeforeSending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &fake{kind: "usb"}
	if _, err := newDriver(t, "LP2844", driver.Options{}).Print(ctx, tr, &driver.Job{Pages: []*image.Gray{white(8, 2)}, Media: dieCut(8, 2)}); err == nil || tr.sent.Len() != 0 {
		t.Errorf("%v, %d bytes sent", err, tr.sent.Len())
	}
}

// The models in zebra-epl.yaml: EPL-only Zebras, each fits its media, none claims a ZPL model's name.
func TestModels(t *testing.T) {
	db := devicedb.Default()
	models := db.Models("epl")
	if len(models) < 6 {
		t.Fatalf("%d epl models", len(models))
	}
	for _, m := range models {
		if m.Vendor != "Zebra" || m.Dialect != "epl2" || (m.DPI != 203 && m.DPI != 300) || m.HeadPins <= 0 || len(m.Media) == 0 ||
			m.QuirkBool("needs_status") || strings.HasSuffix(m.ID, "-Z") {
			t.Errorf("%s: %+v", m.ID, m)
		}
		for _, k := range m.Transports {
			if !slices.Contains([]string{"usb", "tcp", "serial"}, k) {
				t.Errorf("%s: transport %s", m.ID, k)
			}
		}
		for _, md := range db.MediaFor(m) {
			if md.Kind != devicedb.DieCut || md.PrintableWidthDots > m.HeadPins || md.PrintableLengthDots == 0 {
				t.Errorf("%s: media %+v", m.ID, md)
			}
		}
	}
}
