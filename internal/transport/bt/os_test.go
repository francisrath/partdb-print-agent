//go:build linux || windows

package bt

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/francisrath/pdb-print/internal/blecore"
	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/transport"
)

// TestOSErrnos runs the real errno table of this OS (osErrnos, osCases in bt_<os>_test.go).
func TestOSErrnos(t *testing.T) {
	for _, c := range osCases {
		err := mapErr("P", c.errno, osErrnos)
		if got := strings.Contains(err.Error(), "did not answer"); got != c.off {
			t.Errorf("%v: off %v (%v)", c.errno, got, err)
		}
		if got := errors.Is(err, blecore.ErrNoBluetooth); got != c.noAdapter {
			t.Errorf("%v: ErrNoBluetooth %v (%v)", c.errno, got, err)
		}
	}
}

type osCase struct {
	errno          syscall.Errno
	off, noAdapter bool
}

func TestRegistered(t *testing.T) {
	tr, err := transport.New(config.Transport{Kind: "bt", Address: "aa:bb:cc:dd:ee:ff", Name: "RG-MLP80B", Channel: 3}, nil)
	b, ok := tr.(*Transport)
	if err != nil || !ok || b.Address != "aa:bb:cc:dd:ee:ff" || b.Name != "RG-MLP80B" || b.Channel != 3 || b.dial == nil {
		t.Fatalf("transport.New(bt) = %#v, %v", tr, err)
	}
}
