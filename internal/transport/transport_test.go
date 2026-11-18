package transport_test

import (
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/transport"
	_ "github.com/francisrath/partdb-print-agent/internal/transport/tcp"
	_ "github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

func TestTransportRegistry(t *testing.T) {
	for _, c := range []config.Transport{{Kind: "tcp", Host: "h", Port: 9100}, {Kind: "usb", Device: "/dev/usb/lp0"}} {
		tr, err := transport.New(c)
		if err != nil || tr.Kind() != c.Kind {
			t.Errorf("%s: %v %v", c.Kind, tr, err)
		}
	}
	if _, err := transport.New(config.Transport{Kind: "carrier-pigeon"}); err == nil {
		t.Error("unknown kind accepted")
	}
}
