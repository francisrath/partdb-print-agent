package printing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
)

// fakeQL plays a QL-810W on USB: it answers ESC i S with the loaded roll and reports
// "printing completed" after each page. Change the roll with load.
type fakeQL struct {
	mu            sync.Mutex
	width, length byte // mm; length 0 = continuous
	silent        bool // no status replies at all
	queue         []byte
	jobs          int
}

func (f *fakeQL) load(width, length byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.width, f.length = width, length
}

func (f *fakeQL) status(done bool) []byte {
	b := make([]byte, 32)
	copy(b, []byte{0x80, 0x20, 'B', '4', 0x39})
	b[10], b[11], b[17] = f.width, 0x0a, f.length
	if f.length > 0 {
		b[11] = 0x0b
	}
	if done {
		b[18] = 0x01
	}
	return b
}

func (f *fakeQL) Open(context.Context) error { return nil }
func (f *fakeQL) Close() error               { return nil }
func (f *fakeQL) Kind() string               { return "usb" }

func (f *fakeQL) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.silent {
		return len(p), nil
	}
	switch {
	case bytes.HasSuffix(p, []byte{0x1b, 'i', 'S'}):
		f.queue = append(f.queue, f.status(false)...)
	case len(p) > 0 && (p[len(p)-1] == 0x0c || p[len(p)-1] == 0x1a):
		f.queue = append(f.queue, f.status(true)...)
		if p[len(p)-1] == 0x1a {
			f.jobs++
		}
	}
	return len(p), nil
}

func (f *fakeQL) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.queue)
	f.queue = f.queue[n:]
	return n, nil
}

func qlUSB(media string) *config.Printer {
	return &config.Printer{ID: "ql", Model: "QL-810W", Media: media, Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}}
}

func qlService(t *testing.T, f *fakeQL) *Service {
	s := service(t, nil)
	s.OpenTransport = func(*config.Printer) driver.Transport { return f }
	return s
}

func label(wMM, hMM float64) Source { return PDF(pdfgen.Page(wMM, hMM, pdfgen.Text("QL"))) }

func wantMismatch(t *testing.T, err error, detail string) {
	t.Helper()
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.MediaMismatch || !strings.Contains(de.Detail, detail) {
		t.Fatalf("err = %v, want MEDIA_MISMATCH with %q", err, detail)
	}
}

// The roll in the printer decides: after a swap from 29 to 12 mm, a 12 mm label prints with
// no config change, and a 29 mm label is refused with what it needs.
func TestRollChangeBetweenJobs(t *testing.T) {
	f := &fakeQL{width: 29}
	s := qlService(t, f)
	p := qlUSB("dk-22210") // stale config media: now only a fallback
	ctx := context.Background()

	res, err := s.Print(ctx, p, "", label(29, 46))
	if err != nil || res.Media != "dk-22210" || !res.Confirmed {
		t.Fatalf("job 1 on 29 mm: %+v %v", res, err)
	}
	f.load(12, 0)
	res, err = s.Print(ctx, p, "", label(12, 16))
	if err != nil || res.Media != "dk-22214" || !res.Confirmed {
		t.Fatalf("job 2 on 12 mm: %+v %v", res, err)
	}
	_, err = s.Print(ctx, p, "", label(29, 46))
	wantMismatch(t, err, "label is 29×46 mm (needs 29 mm tape), printer has dk-22214 (12 mm tape) loaded")
	if f.jobs != 2 {
		t.Errorf("%d jobs printed, want 2", f.jobs)
	}

	f.load(17, 54) // die-cut, and a landscape PDF of it
	if res, err := s.Print(ctx, p, "", label(54, 17)); err != nil || res.Media != "dk-11204" {
		t.Fatalf("job on 17×54 die-cut: %+v %v", res, err)
	}
}

func TestMediaFallback(t *testing.T) {
	ctx := context.Background()
	f := &fakeQL{silent: true}
	s := qlService(t, f)
	// no status → the config's media
	res, err := s.Print(ctx, qlUSB("dk-22214"), "", label(12, 16))
	if err != nil || res.Media != "dk-22214" {
		t.Fatalf("fallback: %+v %v", res, err)
	}
	// no status and no media in the config → a clear failure, before anything is sent
	for _, media := range []string{"", "auto"} {
		_, err = s.Print(ctx, qlUSB(media), "", label(12, 16))
		if err == nil || !strings.Contains(err.Error(), "set `media:` for this printer as a fallback") {
			t.Errorf("media %q without status: %v", media, err)
		}
	}
}

// A roll seen in the last 30 s stands in for a status read that says nothing; a printer error forgets it.
func TestMediaCache(t *testing.T) {
	ctx := context.Background()
	f := &fakeQL{width: 12}
	s := qlService(t, f)
	p := qlUSB("auto")
	if _, err := s.Print(ctx, p, "", label(12, 16)); err != nil {
		t.Fatal(err)
	}
	f.silent = true
	if res, err := s.Print(ctx, p, "", label(12, 16)); err != nil || res.Media != "dk-22214" {
		t.Fatalf("cached: %+v %v", res, err)
	}
	s.mu.Lock()
	k := s.media["ql"]
	k.at = time.Now().Add(-mediaTTL)
	s.media["ql"] = k
	s.mu.Unlock()
	if _, err := s.Print(ctx, p, "", label(12, 16)); err == nil {
		t.Error("expired cache still used")
	}
	f.silent = false
	if _, err := s.Print(ctx, p, "", label(12, 16)); err != nil {
		t.Fatal(err)
	}
	s.forgetMedia("ql") // what a status or print error does
	if _, _, ok := s.LastMedia("ql"); ok {
		t.Error("forgetMedia kept the roll")
	}
}

func TestMediaAllowed(t *testing.T) {
	f := &fakeQL{width: 12}
	s := qlService(t, f)
	p := qlUSB("auto")
	p.MediaAllowed = []string{"dk-22210", "dk-11204"}
	_, err := s.Print(context.Background(), p, "", label(12, 16))
	wantMismatch(t, err, "printer has dk-22214 (12 mm tape) loaded; this printer takes dk-22210, dk-11204")
	f.load(29, 0)
	if _, err := s.Print(context.Background(), p, "", label(29, 46)); err != nil {
		t.Fatal(err)
	}
}

// Review fixes: a rejected roll replaces the remembered one, the cache honours the allow-list,
// explicit status reads update it, and MediaStatus sees what pickMedia would use.
func TestMediaCacheFollowsObservations(t *testing.T) {
	ctx := context.Background()
	f := &fakeQL{width: 29}
	s := qlService(t, f)
	p := qlUSB("auto")
	p.MediaAllowed = []string{"dk-22210", "dk-22214"}
	if _, err := s.Print(ctx, p, "", label(29, 46)); err != nil {
		t.Fatal(err)
	}
	f.load(17, 54) // not allowed: refused, and dk-22210 is no longer assumed
	_, err := s.Print(ctx, p, "", label(29, 46))
	wantMismatch(t, err, "this printer takes")
	f.silent = true
	if _, err := s.Print(ctx, p, "", label(29, 46)); err == nil {
		t.Error("silent printer after a rejected roll printed on the old one")
	}

	f.silent = false
	f.load(12, 0)
	if _, err := s.Status(ctx, p); err != nil { // an explicit status read records the roll
		t.Fatal(err)
	}
	if m, _, ok := s.LastMedia("ql"); !ok || m.ID != "dk-22214" {
		t.Fatalf("status read not recorded: %v %v", m.ID, ok)
	}
	if _, text, ok := s.MediaStatus(p, &driver.Status{Code: driver.OK}); !ok || text != "dk-22214 (detected earlier)" {
		t.Errorf("MediaStatus without a reported roll: %q %v", text, ok)
	}
	p.MediaAllowed = []string{"dk-22210"} // allow-list edited: the cached roll no longer counts
	if _, _, ok := s.MediaStatus(p, &driver.Status{Code: driver.OK}); ok {
		t.Error("cached roll outside media_allowed still offered")
	}
}
