package blecore

import (
	"errors"
	"reflect"
	"strings"
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
