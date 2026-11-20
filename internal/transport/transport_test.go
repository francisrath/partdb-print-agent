package transport_test

import (
	"sync"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

func TestTransportRegistry(t *testing.T) {
	tr, err := transport.New(config.Transport{Kind: "tcp", Host: "printer.lan", Port: 9101}, nil)
	if tc, ok := tr.(*tcp.Transport); err != nil || !ok || tc.Addr != "printer.lan:9101" {
		t.Errorf("tcp: %#v %v", tr, err)
	}
	tr, err = transport.New(config.Transport{Kind: "usb", Device: "/dev/usb/lp3"}, nil)
	if u, ok := tr.(*usb.Transport); err != nil || !ok || u.Device != "/dev/usb/lp3" {
		t.Errorf("usb: %#v %v", tr, err)
	}
	tr, err = transport.New(config.Transport{Kind: "usb", USBID: "04f9:209c", Serial: "X1"}, nil)
	if u, ok := tr.(*usb.Transport); err != nil || !ok || u.USBID != "04f9:209c" || u.Serial != "X1" {
		t.Errorf("usb by id: %#v %v", tr, err)
	}
	if _, err := transport.New(config.Transport{Kind: "carrier-pigeon"}, nil); err == nil {
		t.Error("unknown kind accepted")
	}
}

var (
	probeOnce sync.Once
	probeGot  *devicedb.Model
)

func TestRegisterPassesModel(t *testing.T) {
	probeGot = nil
	// the registry is global and rejects duplicates, so register once per process (-count=N)
	probeOnce.Do(func() {
		transport.Register("model-probe", func(c config.Transport, m *devicedb.Model) (driver.Transport, error) {
			probeGot = m
			return nil, nil
		})
	})
	want := &devicedb.Model{ID: "x"}
	if _, err := transport.New(config.Transport{Kind: "model-probe"}, want); err != nil || probeGot != want {
		t.Errorf("model not passed: %p %p %v", probeGot, want, err)
	}
}
