package blecore

import (
	"errors"
	"strings"
	"time"
)

// Settle is how long a name-only scan keeps listening after the first advert with that name, so a second
// printer with the same name shows up and Pick reports the ambiguity.
const Settle = 2 * time.Second

// Scanned collects a scan's adverts, one per address. The zero value is ready to use.
type Scanned struct {
	ads   []Advert
	named []time.Time // when each entry first carried a name
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
	if ad.Name != "" && s.named[i].IsZero() {
		s.named[i] = now
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
	var first time.Time
	for i, a := range s.ads {
		if a.Name == name && (first.IsZero() || s.named[i].Before(first)) {
			first = s.named[i]
		}
	}
	return !first.IsZero() && now.Sub(first) >= Settle
}
