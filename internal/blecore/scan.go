package blecore

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Settle is how long a name-only scan keeps listening after the first advert with that name, so a second
// printer with the same name shows up and Pick reports the ambiguity.
const Settle = 2 * time.Second

// Scanned collects a scan's adverts, one per address. The zero value is ready to use.
type Scanned struct {
	ads   []Advert
	named []time.Time // when each entry first carried its current name
	index map[string]int
}

// Add merges an advert: the latest one wins, but keeps the earlier name and services when it has none
// (RSSI updates and scan responses often lack them).
func (s *Scanned) Add(ad Advert, now time.Time) {
	key := strings.ToUpper(ad.Address)
	i, ok := s.index[key]
	if !ok {
		if s.index == nil {
			s.index = map[string]int{}
		}
		i = len(s.ads)
		s.index[key] = i
		s.ads = append(s.ads, Advert{})
		s.named = append(s.named, time.Time{})
	}
	old := s.ads[i]
	if ad.Name == "" {
		ad.Name = old.Name
	}
	if ad.Services == nil {
		ad.Services = old.Services
	}
	if ad.Name != "" && (s.named[i].IsZero() || ad.Name != old.Name) {
		s.named[i] = now // a renamed device starts its settle window again
	}
	s.ads[i] = ad
}

// Adverts returns a copy of everything seen.
func (s *Scanned) Adverts() []Advert { return append([]Advert(nil), s.ads...) }

// Settled reports whether a scan for the configured printer can stop: at once when the address has been
// seen (or the config can't match anything), and Settle after the first advert with the name when only
// the name is set. The caller then runs Pick on Adverts().
func (s *Scanned) Settled(name, address string, now time.Time) bool {
	if address != "" || name == "" {
		_, err := Pick(s.ads, name, address)
		return !errors.Is(err, ErrNotFound)
	}
	first := s.first(name)
	return !first.IsZero() && now.Sub(first) >= Settle
}

// pending reports a name-only scan whose name has been seen but whose settle window is still open.
func (s *Scanned) pending(name, address string, now time.Time) bool {
	return address == "" && name != "" && !s.first(name).IsZero() && !s.Settled(name, address, now)
}

// first is when the earliest device with name first carried it; zero if none has.
func (s *Scanned) first(name string) time.Time {
	var first time.Time
	for i, a := range s.ads {
		if a.Name == name && (first.IsZero() || s.named[i].Before(first)) {
			first = s.named[i]
		}
	}
	return first
}

// Target is the printer an Open scan looks for (see Settled).
type Target struct{ Name, Address string }

// Radio is the scanning half of a Bluetooth library.
type Radio struct {
	Scan func(found func(Advert)) error // runs until Stop or a failure
	Stop func() error
	// Fatal picks the errors Scan returns while being stopped that still fail the scan (an adapter that went
	// away at the deadline); others don't spoil what was found. nil: none do.
	Fatal func(error) bool
}

// Run scans until timeout, ctx ends, or (with a target) Settled reports true, and returns every device seen
// once (by address). A name-only target first seen near the timeout gets one extra Settle to finish its
// window; still unsettled after that, Run fails rather than let Pick choose from half a window. A scan
// that ends by itself with an error returns that error as the library gave it.
func (r Radio) Run(ctx context.Context, timeout time.Duration, target *Target) ([]Advert, error) {
	var (
		mu   sync.Mutex
		seen Scanned
	)
	errc := make(chan error, 1)
	go func() {
		errc <- noDoubleClose(func() error {
			return r.Scan(func(ad Advert) {
				mu.Lock()
				seen.Add(ad, time.Now())
				mu.Unlock()
			})
		})
	}()
	is := func(f func(*Scanned, string, string, time.Time) bool) bool {
		mu.Lock()
		defer mu.Unlock()
		return target != nil && f(&seen, target.Name, target.Address, time.Now())
	}
	stop := func() ([]Advert, error) {
		if err := r.stopWait(errc); err != nil && r.Fatal != nil && r.Fatal(err) {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		return seen.Adverts(), nil
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	extended := false
	for {
		select {
		case err := <-errc: // the scan ended by itself
			if err != nil {
				// Linux (v0.17.0) keeps its scan state after a failed start ("a scan is already in
				// progress" from then on); stopping clears it.
				_ = noDoubleClose(r.Stop)
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			return seen.Adverts(), nil
		case <-ctx.Done():
			return stop()
		case <-deadline.C:
			if !is((*Scanned).pending) {
				return stop()
			}
			if extended {
				if _, err := stop(); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("a device named %q showed up only at the end of the scan; run the job again", target.Name)
			}
			extended = true
			deadline.Reset(Settle)
		case <-tick.C:
			if is((*Scanned).Settled) {
				return stop()
			}
		}
	}
}

// stopWait stops the scan and waits for Scan to return, returning its error. StopScan fails while Scan is
// still starting and may be lost on Windows before the watcher has started, so it is repeated until Scan
// returns. Once stopped, v0.17.0 returns promptly: BlueZ's loop sees the closed cancel channel at once,
// Windows waits for the watcher's Stopped event (which carries an adapter failure).
func (r Radio) stopWait(errc <-chan error) error {
	for {
		_ = noDoubleClose(r.Stop)
		select {
		case err := <-errc:
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// noDoubleClose turns a "close of closed channel" panic into an error. v0.17.0's Linux StopScan and the
// power-off branch of its Scan both close the scan's cancel channel without a lock, so whichever loses that
// race panics.
// ponytail: patches around a library race; drop once tinygo bluetooth guards scanCancelChan.
func noDoubleClose(f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if re, ok := p.(runtime.Error); ok && strings.Contains(re.Error(), "close of closed channel") {
				err = fmt.Errorf("scan: %w", re)
				return
			}
			panic(p)
		}
	}()
	return f()
}
