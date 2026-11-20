package blecore

import (
	"errors"
	"strings"
	"testing"
)

var adverts = []Advert{
	{Name: "MX10", Address: "AA:BB:CC:00:00:01", RSSI: -60},
	{Name: "MX10", Address: "AA:BB:CC:00:00:02", RSSI: -70},
	{Name: "GB01", Address: "AA:BB:CC:00:00:03", RSSI: -50},
}

func TestPickAmbiguousName(t *testing.T) {
	_, err := Pick(adverts, "MX10", "")
	if err == nil {
		t.Fatal("two MX10s: want an error")
	}
	for _, a := range []string{"AA:BB:CC:00:00:01", "AA:BB:CC:00:00:02"} {
		if !strings.Contains(err.Error(), a) {
			t.Errorf("error %q does not name %s", err, a)
		}
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("ambiguous is not 'not found'")
	}
}

func TestPickByAddress(t *testing.T) {
	tests := []struct {
		name, pickName, address, want string
		notFound                      bool
	}{
		{"address only", "", "AA:BB:CC:00:00:02", "AA:BB:CC:00:00:02", false},
		{"address case-insensitive", "", "aa:bb:cc:00:00:02", "AA:BB:CC:00:00:02", false},
		{"address wins over an ambiguous name", "MX10", "AA:BB:CC:00:00:01", "AA:BB:CC:00:00:01", false},
		{"address wins over another device's name", "GB01", "AA:BB:CC:00:00:01", "AA:BB:CC:00:00:01", false},
		{"unique name", "GB01", "", "AA:BB:CC:00:00:03", false},
		{"address not seen", "GB01", "AA:BB:CC:00:00:09", "", true},
		{"name not seen", "X6", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Pick(adverts, tt.pickName, tt.address)
			if tt.notFound {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Address != tt.want {
				t.Fatalf("picked %s, want %s", got.Address, tt.want)
			}
		})
	}
	if _, err := Pick(adverts, "", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("no name or address: err %v, want a config error", err)
	}
}
