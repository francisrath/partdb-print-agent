package blecore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func TestScannedMerge(t *testing.T) {
	var s Scanned
	s.Add(Advert{Name: "MX10", Address: "AA:00", RSSI: -70, Services: []string{"ae30"}}, t0)
	s.Add(Advert{Address: "AA:00", RSSI: -60}, t0.Add(time.Second)) // RSSI update without name/services
	s.Add(Advert{Name: "GB01", Address: "BB:00", RSSI: -50}, t0.Add(time.Second))
	s.Add(Advert{Name: "MX10", Address: "AA:00", RSSI: -65, Services: []string{"ae30", "180a"}}, t0.Add(2*time.Second))
	want := []Advert{
		{Name: "MX10", Address: "AA:00", RSSI: -65, Services: []string{"ae30", "180a"}},
		{Name: "GB01", Address: "BB:00", RSSI: -50},
	}
	if got := s.Adverts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	got := s.Adverts()
	got[0].Name = "changed"
	if s.Adverts()[0].Name != "MX10" {
		t.Fatal("Adverts must return a copy")
	}
}

func TestScannedSettled(t *testing.T) {
	t.Run("two same-name adverts 1 s apart are ambiguous", func(t *testing.T) {
		var s Scanned
		s.Add(Advert{Name: "MX10", Address: "AA:01"}, t0)
		if s.Settled("MX10", "", t0) {
			t.Fatal("stopped at the first name match")
		}
		s.Add(Advert{Name: "MX10", Address: "AA:02"}, t0.Add(time.Second))
		if s.Settled("MX10", "", t0.Add(Settle-time.Millisecond)) {
			t.Fatal("stopped inside the settle window")
		}
		if !s.Settled("MX10", "", t0.Add(Settle)) {
			t.Fatal("did not stop after the settle window")
		}
		_, err := Pick(s.Adverts(), "MX10", "")
		if err == nil || !strings.Contains(err.Error(), "AA:01") || !strings.Contains(err.Error(), "AA:02") {
			t.Fatalf("err %v, want ambiguity naming both", err)
		}
	})
	t.Run("one name match and nothing else", func(t *testing.T) {
		var s Scanned
		s.Add(Advert{Name: "GB01", Address: "BB:00"}, t0)
		s.Add(Advert{Address: "AA:01"}, t0.Add(100*time.Millisecond)) // name arrives in a later scan response
		s.Add(Advert{Name: "MX10", Address: "AA:01"}, t0.Add(500*time.Millisecond))
		if s.Settled("MX10", "", t0.Add(500*time.Millisecond+Settle-time.Millisecond)) {
			t.Fatal("settle window counted from before the name was seen")
		}
		if !s.Settled("MX10", "", t0.Add(500*time.Millisecond+Settle)) {
			t.Fatal("did not stop after the settle window")
		}
		got, err := Pick(s.Adverts(), "MX10", "")
		if err != nil || got.Address != "AA:01" {
			t.Fatalf("picked %+v, %v", got, err)
		}
	})
	t.Run("address stops at its advert", func(t *testing.T) {
		var s Scanned
		s.Add(Advert{Name: "MX10", Address: "AA:01"}, t0)
		if s.Settled("MX10", "AA:02", t0.Add(time.Hour)) {
			t.Fatal("stopped without the configured address")
		}
		s.Add(Advert{Name: "MX10", Address: "aa:02"}, t0.Add(time.Second))
		if !s.Settled("MX10", "AA:02", t0.Add(time.Second)) {
			t.Fatal("did not stop at the address")
		}
	})
	t.Run("nothing configured stops at once", func(t *testing.T) {
		var s Scanned
		if !s.Settled("", "", t0) {
			t.Fatal("empty config should stop the scan")
		}
		if _, err := Pick(nil, "", ""); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestScannedNameChangeRestartsSettle(t *testing.T) {
	var s Scanned
	s.Add(Advert{Name: "GB01", Address: "AA:01"}, t0)
	s.Add(Advert{Name: "MX10", Address: "AA:01"}, t0.Add(3*time.Second))
	if s.Settled("MX10", "", t0.Add(3*time.Second)) {
		t.Fatal("settle window counted from the old name")
	}
	s.Add(Advert{Address: "AA:01"}, t0.Add(4*time.Second))               // nameless update keeps the time
	s.Add(Advert{Name: "MX10", Address: "AA:01"}, t0.Add(4*time.Second)) // same name keeps the time
	if !s.Settled("MX10", "", t0.Add(3*time.Second+Settle)) {
		t.Fatal("same-name or nameless update restarted the window")
	}
}

// fakeRadio delivers adverts at given offsets, then blocks until stopped.
type fakeRadio struct {
	at       []time.Duration
	ads      []Advert
	startErr error // Scan fails at once with this
	stopErr  error // Scan returns this once stopped
	stops    chan struct{}
	stopped  chan struct{}
	once     sync.Once
}

func newFakeRadio() *fakeRadio {
	return &fakeRadio{stops: make(chan struct{}, 100), stopped: make(chan struct{})}
}

func (f *fakeRadio) radio(fatal func(error) bool) Radio {
	return Radio{
		Scan: func(found func(Advert)) error {
			if f.startErr != nil {
				return f.startErr
			}
			start := time.Now()
			for i, ad := range f.ads {
				select {
				case <-f.stopped:
					return f.stopErr
				case <-time.After(f.at[i] - time.Since(start)):
					found(ad)
				}
			}
			<-f.stopped
			return f.stopErr
		},
		Stop: func() error {
			f.stops <- struct{}{}
			f.once.Do(func() { close(f.stopped) })
			return nil
		},
		Fatal: fatal,
	}
}

func TestRadioRun(t *testing.T) {
	ctx := context.Background()
	t.Run("name match at the deadline settles before returning", func(t *testing.T) {
		f := newFakeRadio()
		f.at = []time.Duration{80 * time.Millisecond, Settle / 2}
		f.ads = []Advert{{Name: "MX10", Address: "AA:01"}, {Name: "MX10", Address: "AA:02"}}
		start := time.Now()
		got, err := f.radio(nil).Run(ctx, 100*time.Millisecond, &Target{Name: "MX10"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %+v, want both same-name printers (the second came inside the window)", got)
		}
		if d := time.Since(start); d < Settle {
			t.Fatalf("returned after %v, before the settle window ended", d)
		}
	})
	t.Run("nothing found stops at the deadline", func(t *testing.T) {
		f := newFakeRadio()
		start := time.Now()
		got, err := f.radio(nil).Run(ctx, 100*time.Millisecond, &Target{Name: "MX10"})
		if err != nil || len(got) != 0 {
			t.Fatalf("got %+v, %v", got, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("waited %v without a match", d)
		}
	})
	t.Run("address stops at once", func(t *testing.T) {
		f := newFakeRadio()
		f.at = []time.Duration{10 * time.Millisecond}
		f.ads = []Advert{{Name: "MX10", Address: "AA:01"}}
		got, err := f.radio(nil).Run(ctx, 5*time.Second, &Target{Address: "aa:01"})
		if err != nil || len(got) != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("failed start clears the library's scan state", func(t *testing.T) {
		f := newFakeRadio()
		f.startErr = errors.New("SetDiscoveryFilter failed")
		_, err := f.radio(nil).Run(ctx, time.Second, nil)
		if err != f.startErr {
			t.Fatalf("err %v", err)
		}
		if len(f.stops) == 0 {
			t.Fatal("Stop not called after the failed start")
		}
	})
	t.Run("fatal error while stopping at the deadline", func(t *testing.T) {
		f := newFakeRadio()
		f.stopErr = errors.New("failed to stop scanning (error code 1)")
		fatal := func(err error) bool { return strings.Contains(err.Error(), "code 1") }
		if _, err := f.radio(fatal).Run(ctx, 50*time.Millisecond, nil); err != f.stopErr {
			t.Fatalf("err %v, want the adapter error", err)
		}
	})
	t.Run("other error while stopping keeps the adverts", func(t *testing.T) {
		f := newFakeRadio()
		f.at = []time.Duration{10 * time.Millisecond}
		f.ads = []Advert{{Name: "GB01", Address: "AA:01"}}
		f.stopErr = errors.New("No discovery started")
		fatal := func(err error) bool { return false }
		got, err := f.radio(fatal).Run(ctx, 50*time.Millisecond, nil)
		if err != nil || len(got) != 1 {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("a double close in the library doesn't panic", func(t *testing.T) {
		ch := make(chan struct{})
		close(ch)
		r := Radio{
			Scan: func(func(Advert)) error { close(ch); return nil }, // the library's power-off branch lost the race
			Stop: func() error { return nil },
		}
		if _, err := r.Run(ctx, time.Second, nil); err == nil {
			t.Fatal("want an error from the recovered close")
		}
		r = Radio{
			Scan: func(func(Advert)) error { time.Sleep(300 * time.Millisecond); return nil },
			Stop: func() error { close(ch); return nil }, // our StopScan lost the race
		}
		if _, err := r.Run(ctx, 10*time.Millisecond, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// tinygo's Linux Scan keeps its state after a failed start: only a Stop lets the next scan run.
func TestRadioRunAfterFailedStart(t *testing.T) {
	var scanning, fail atomic.Bool
	r := Radio{
		Scan: func(found func(Advert)) error {
			if !scanning.CompareAndSwap(false, true) {
				return errors.New("bluetooth: a scan is already in progress")
			}
			if fail.Load() {
				return errors.New("SetDiscoveryFilter failed") // the state stays set, as in v0.17.0
			}
			found(Advert{Name: "MX10", Address: "AA:01"})
			for scanning.Load() {
				time.Sleep(time.Millisecond)
			}
			return nil
		},
		Stop: func() error { scanning.Store(false); return nil },
	}
	fail.Store(true)
	if _, err := r.Run(context.Background(), time.Second, nil); err == nil || !strings.Contains(err.Error(), "SetDiscoveryFilter") {
		t.Fatalf("failed start: %v", err)
	}
	fail.Store(false)
	got, err := r.Run(context.Background(), 50*time.Millisecond, nil)
	if err != nil || len(got) != 1 || got[0].Address != "AA:01" {
		t.Fatalf("second scan: %+v, %v", got, err)
	}
}
