package transport_test

import (
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/transport"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

func TestTransportRegistry(t *testing.T) {
	tr, err := transport.New(config.Transport{Kind: "tcp", Host: "printer.lan", Port: 9101})
	if tc, ok := tr.(*tcp.Transport); err != nil || !ok || tc.Addr != "printer.lan:9101" {
		t.Errorf("tcp: %#v %v", tr, err)
	}
	tr, err = transport.New(config.Transport{Kind: "usb", Device: "/dev/usb/lp3"})
	if u, ok := tr.(*usb.Transport); err != nil || !ok || u.Device != "/dev/usb/lp3" {
		t.Errorf("usb: %#v %v", tr, err)
	}
	tr, err = transport.New(config.Transport{Kind: "usb", USBID: "04f9:209c", Serial: "X1"})
	if u, ok := tr.(*usb.Transport); err != nil || !ok || u.USBID != "04f9:209c" || u.Serial != "X1" {
		t.Errorf("usb by id: %#v %v", tr, err)
	}
	if _, err := transport.New(config.Transport{Kind: "carrier-pigeon"}); err == nil {
		t.Error("unknown kind accepted")
	}
}
