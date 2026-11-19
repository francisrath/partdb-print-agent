package zpl_test

import (
	"context"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
)

func gk420d(t *testing.T) (driver.Driver, *devicedb.Model) {
	t.Helper()
	m, ok := devicedb.Default().Model("GK420d")
	if !ok {
		t.Fatal("no GK420d")
	}
	d, err := driver.New(m, driver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return d, m
}

func media(t *testing.T, m *devicedb.Model, id string) *driver.Media {
	for _, md := range devicedb.Default().MediaFor(m) {
		if md.ID == id {
			return &md
		}
	}
	t.Fatalf("no media %s", id)
	return nil
}

func page(m *driver.Media) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, m.PrintableWidthDots, m.PrintableLengthDots))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	img.Pix[0] = 0
	return img
}

func TestStatusIdle(t *testing.T) {
	d, _ := gk420d(t)
	fake := &zpltest.Fake{}
	st, err := d.Status(context.Background(), tcp.New(fake.Start(t)))
	if err != nil || st.Code != driver.OK || st.Media == nil || st.Media.ID != "unknown" || st.Media.PrintableLengthDots != 812 || !st.Idle {
		t.Fatalf("%+v %+v %v", st, st.Media, err)
	}
}

func TestStatusErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		fake *zpltest.Fake
		want driver.Code
	}{
		"head open (~HQES)":       {&zpltest.Fake{Errors: 0x4}, driver.CoverOpen},
		"paused (~HS, old fw)":    {&zpltest.Fake{Paused: true, NoHQES: true}, driver.Paused},
		"paper out (~HS, old fw)": {&zpltest.Fake{PaperOut: true, NoHQES: true}, driver.NoMedia},
		"head up (~HS, old fw)":   {&zpltest.Fake{HeadUp: true, NoHQES: true}, driver.CoverOpen},
	} {
		d, _ := gk420d(t)
		st, err := d.Status(context.Background(), tcp.New(tc.fake.Start(t)))
		if err != nil || st.Code != tc.want || st.Detail == "" {
			t.Errorf("%s: %+v %v, want %s", name, st, err, tc.want)
		}
	}
}

// A Zebra with its head open doesn't answer ~HS at all; ~HQES names the problem.
func TestStatusSilentHS(t *testing.T) {
	d, _ := gk420d(t)
	fake := &zpltest.Fake{SilentHS: true, Errors: 0x4}
	start := time.Now()
	st, err := d.Status(context.Background(), tcp.New(fake.Start(t)))
	if err != nil || st.Code != driver.CoverOpen || time.Since(start) > 3*time.Second {
		t.Errorf("%+v %v after %v", st, err, time.Since(start))
	}
	fake.Set(func(f *zpltest.Fake) { f.NoHQES, f.Errors = true, 0 })
	_, err = d.Status(context.Background(), tcp.New(fake.Start(t)))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError || !strings.Contains(de.Detail, "head open") {
		t.Errorf("silent printer: %v", err)
	}
}

func TestPrintConfirms(t *testing.T) {
	defer zpl.SetTiming(20*time.Millisecond, 500*time.Millisecond)()
	d, m := gk420d(t)
	fake := &zpltest.Fake{Hold: 100 * time.Millisecond}
	tr := tcp.New(fake.Start(t))
	st, _ := d.Status(context.Background(), tr)
	md := media(t, m, "label-102x102")
	res, err := d.Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{page(md), page(md)}, Media: md, Status: st})
	if err != nil || !res.Confirmed || res.Pages != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	f := fake.Formats()
	if len(f) != 2 || !strings.Contains(f[0], "^PON^LH0,0^LS0^PW816^LL816^FO0,0^GFA,") {
		t.Errorf("formats %d: %.80q", len(f), f)
	}
}

// ~HS can show an empty buffer before the job got there: that alone isn't a confirmation.
func TestPrintConfirmsOnlyAfterSeen(t *testing.T) {
	defer zpl.SetTiming(20*time.Millisecond, 300*time.Millisecond)()
	d, m := gk420d(t)
	fake := &zpltest.Fake{Invisible: true}
	tr := tcp.New(fake.Start(t))
	md := media(t, m, "label-102x102")
	res, err := d.Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{page(md)}, Media: md})
	if err != nil || res.Confirmed || !strings.Contains(res.Message, "probably printed") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestPrintLengthMismatch(t *testing.T) {
	d, m := gk420d(t)
	fake := &zpltest.Fake{} // 812 dots = 101.5 mm loaded
	tr := tcp.New(fake.Start(t))
	st, _ := d.Status(context.Background(), tr)
	md := media(t, m, "label-102x76")
	_, err := d.Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{page(md)}, Media: md, Status: st})
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.MediaMismatch || len(fake.Formats()) != 0 {
		t.Errorf("err %v, %d formats sent", err, len(fake.Formats()))
	}
}

// An error while the label is being printed is reported, not "sent".
func TestPrintErrorWhilePrinting(t *testing.T) {
	defer zpl.SetTiming(20*time.Millisecond, 2*time.Second)()
	d, m := gk420d(t)
	fake := &zpltest.Fake{Hold: time.Hour}
	tr := tcp.New(fake.Start(t))
	md := media(t, m, "label-102x102")
	time.AfterFunc(150*time.Millisecond, func() { fake.Set(func(f *zpltest.Fake) { f.PaperOut, f.Errors = true, 0x1 }) })
	_, err := d.Print(context.Background(), tr, &driver.Job{Pages: []*image.Gray{page(md)}, Media: md})
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.NoMedia {
		t.Errorf("err %v", err)
	}
}
