package bt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

func TestResolve(t *testing.T) {
	devs := []device{
		{"RG-MLP80B", "AA:BB:CC:00:00:01"},
		{"MLP", "AA:BB:CC:00:00:01"}, // the alias of the same device
		{"Twin", "AA:BB:CC:00:00:02"},
		{"Twin", "aa:bb:cc:00:00:03"},
		{"Solo", "AA:BB:CC:00:00:04"},
		{"Solo", "aa:bb:cc:00:00:04"}, // the same device seen by two adapters
	}
	tests := []struct {
		name, want, errHas string
	}{
		{"RG-MLP80B", "AA:BB:CC:00:00:01", ""},
		{"MLP", "AA:BB:CC:00:00:01", ""},
		{"Solo", "AA:BB:CC:00:00:04", ""},
		{"Twin", "", `two Bluetooth devices are called "Twin"; set address`},
		{"Nope", "", "switch it on and pair it"},
		{"", "", "set the printer's address or name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(devs, tt.name)
			if tt.errHas == "" {
				if err != nil || got != tt.want {
					t.Fatalf("resolve = %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errHas) {
				t.Fatalf("resolve err %v, want one containing %q", err, tt.errHas)
			}
		})
	}
}

func TestMapErr(t *testing.T) {
	// plain numbers: the real per-OS tables are in bt_linux.go / bt_windows.go
	tab := errnos{off: []syscall.Errno{112, 111}, noAdapter: []syscall.Errno{19}}
	tests := []struct {
		name     string
		err      error
		off, no  bool
		passThru bool
	}{
		{"host down", syscall.Errno(112), true, false, false},
		{"refused, wrapped", fmt.Errorf("connect: %w", syscall.Errno(111)), true, false, false},
		{"our timeout", os.ErrDeadlineExceeded, true, false, false},
		{"no adapter", syscall.Errno(19), false, true, false},
		{"other", syscall.Errno(1), false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapErr("RG-MLP80B (AA:BB:CC:DD:EE:FF)", tt.err, tab)
			msg := err.Error()
			if got := strings.Contains(msg, "did not answer – is it switched on and paired with this computer?"); got != tt.off {
				t.Errorf("off message %v: %q", got, msg)
			}
			if got := errors.Is(err, blecore.ErrNoBluetooth); got != tt.no {
				t.Errorf("ErrNoBluetooth %v: %q", got, msg)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("does not wrap %v: %q", tt.err, msg)
			}
			if !strings.HasPrefix(msg, "bt: ") {
				t.Errorf("no bt: prefix: %q", msg)
			}
		})
	}
}

func pipeTransport(t *testing.T) (*Transport, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	tr := &Transport{ReadTimeout: 50 * time.Millisecond, dial: func(context.Context) (conn, error) { return a, nil }}
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close(); _ = b.Close() })
	return tr, b
}

func TestTransportOverPipe(t *testing.T) {
	tr, peer := pipeTransport(t)
	if tr.Kind() != "bt" {
		t.Errorf("Kind = %q", tr.Kind())
	}

	payload := make([]byte, 10000)
	for i := range payload {
		payload[i] = byte(i)
	}
	got := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(peer, int64(len(payload))))
		got <- b
	}()
	if n, err := tr.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if b := <-got; string(b) != string(payload) {
		t.Fatalf("peer got %d bytes, want %d", len(b), len(payload))
	}

	p := make([]byte, 4)
	if _, err := tr.Read(p); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent Read err %v, want os.ErrDeadlineExceeded", err)
	}
	go func() { _, _ = peer.Write([]byte{0x12}) }()
	if n, err := tr.Read(p); n != 1 || err != nil || p[0] != 0x12 {
		t.Fatalf("Read = %d %X %v", n, p[:n], err)
	}

	_ = peer.Close()
	if _, err := tr.Read(p); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after the peer hung up: %v, want a dropped-link error", err)
	}

	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := tr.Write([]byte{1}); err == nil {
		t.Error("Write after Close succeeded")
	}
	if _, err := tr.Read(p); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Read after Close: %v", err)
	}
}

func TestOpenDialError(t *testing.T) {
	want := errors.New("boom")
	tr := &Transport{dial: func(context.Context) (conn, error) { return nil, want }}
	if err := tr.Open(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Open = %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close after a failed Open: %v", err)
	}
}
