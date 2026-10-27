package snmp

import (
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func TestLoadedMedia(t *testing.T) {
	tests := []struct {
		name string
		kind driver.MediaKind
		w, l int
		ok   bool
	}{
		{`29mm / 1.1"`, driver.Continuous, 29, 0, true}, // seen on a QL-810W with DK-22210
		{`12mm / 0.47"`, driver.Continuous, 12, 0, true},
		{`17mm x 54mm / 0.66" x 2.1"`, driver.DieCut, 17, 54, true},
		{"", "", 0, 0, false},
		{"Media", "", 0, 0, false},
	}
	for _, tc := range tests {
		s := &State{MediaName: tc.name}
		kind, w, l, ok := s.LoadedMedia()
		if kind != tc.kind || w != tc.w || l != tc.l || ok != tc.ok {
			t.Errorf("%q: got %v %d %d %v", tc.name, kind, w, l, ok)
		}
	}
}

func TestProblem(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  driver.Code
	}{
		{"ready", State{Idle: true}, ""},
		{"media low is only a warning", State{Idle: true, Alerts: []int{807}}, ""},
		// observed: wrong roll → hrDeviceStatus down, display ERROR, alert 802
		{"wrong roll", State{Down: true, Display: "ERROR", Alerts: []int{802}}, driver.MediaMismatch},
		{"cover open", State{Down: true, Alerts: []int{3}}, driver.CoverOpen},
		{"empty", State{Down: true, Alerts: []int{808}}, driver.NoMedia},
		{"down without a known alert", State{Down: true, Display: "ERROR"}, driver.Unknown},
	}
	for _, tc := range tests {
		p := tc.state.Problem()
		if (p == nil && tc.want != "") || (p != nil && p.Code != tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, p, tc.want)
		}
	}
}
