package bt

import (
	"errors"
	"net"
	"testing"
	"time"

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

// BTH_QUERY_DEVICE is packed to 5 bytes and its LAP is reserved: zero.
func TestInquiryBlob(t *testing.T) {
	if got, want := inquiryBlob(8), [5]byte{0, 0, 0, 0, 8}; got != want {
		t.Errorf("% x, want % x", got, want)
	}
}

// Winsock may hand a closed handle out again at once, so Close during a pending call must not release it
// under that call: Close ends the Read, then the handle is closed, and later
// calls fail without touching it. A loopback TCP socket stands in for RFCOMM.
func TestSocketCloseDuringRead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	h, err := windows.Socket(windows.AF_INET, windows.SOCK_STREAM, windows.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.Connect(h, &windows.SockaddrInet4{Port: ln.Addr().(*net.TCPAddr).Port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s := &socket{h: h}
	if err := s.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = s.Read(make([]byte, 1))
	}()
	time.Sleep(50 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-read:
	case <-time.After(time.Second): // well before the read timeout
		t.Fatal("Read didn't end after Close")
	}
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read after Close: %v, want net.ErrClosed", err)
	}
	if _, err := windows.GetsockoptInt(h, windows.SOL_SOCKET, windows.SO_RCVBUF); err == nil {
		t.Error("the handle is still open after the Read ended")
	}
}
