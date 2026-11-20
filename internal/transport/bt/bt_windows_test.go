package bt

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

var osErrnos = winErrnos

var osCases = []osCase{
	{windows.WSAETIMEDOUT, true, false},
	{windows.WSAECONNREFUSED, true, false},
	{windows.WSAEHOSTDOWN, true, false},
	{windows.WSAEAFNOSUPPORT, false, true},
	{windows.WSAENETDOWN, false, true},
	{windows.WSAEACCES, true, false},
	{windows.WSAEADDRNOTAVAIL, false, false}, // no SPP service: its own message (TestMapErr)
	{windows.WSAEINVAL, false, false},
}

func TestWinNoSPP(t *testing.T) {
	if !isAny(windows.WSAEADDRNOTAVAIL, winErrnos.noSPP) {
		t.Error("WSAEADDRNOTAVAIL is not the no-SPP error")
	}
}

func TestWinLookupErrs(t *testing.T) {
	for _, e := range []windows.Errno{windows.WSASERVICE_NOT_FOUND, windows.WSA_E_NO_MORE, windows.WSAENOMORE} {
		if devs, err := lookupErr(e, winErrnos); devs != nil || err != nil {
			t.Errorf("%v: %v %v, want an empty list", e, devs, err)
		}
	}
	if _, err := lookupErr(windows.WSAEINVAL, winErrnos); !errors.Is(err, blecore.ErrNoBluetooth) {
		t.Errorf("WSAEINVAL: %v, want ErrNoBluetooth", err)
	}
}
