//go:build darwin

package ble

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blebridge"
	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/transport"
)

const fakeEnv = "PDB_PRINT_BLE_FAKE"

// TestMain doubles as the fake pdb-print-ble: the tests put a symlink to this test binary named
// pdb-print-ble on PATH and pick its behaviour through fakeEnv.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		if err := fakeHelper(mode, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHelper modes: "ok" echoes the open request and every write back as notifications; "die" exits on the
// first write; "fail" refuses the open; "scan" / "scan-nobt" answer the scan mode.
func fakeHelper(mode string, args []string) error {
	switch mode {
	case "scan":
		if strings.Join(args, " ") != "scan --timeout 8s" {
			return fmt.Errorf("args %q", args)
		}
		for _, ad := range []blecore.Advert{
			{Name: "MX10", Address: "5E3F0A6B-0000-4000-8000-000000000001", RSSI: -60, Services: []string{"0000ae30-0000-1000-8000-00805f9b34fb"}},
			{Name: "", Address: "5E3F0A6B-0000-4000-8000-000000000002", RSSI: -90},
		} {
			b, _ := json.Marshal(ad)
			fmt.Println(string(b))
		}
		return nil
	case "scan-nobt":
		return errors.New("no Bluetooth adapter: bluetooth is powered off")
	}
	if len(args) != 0 {
		return fmt.Errorf("bridge mode got args %q", args)
	}
	typ, p, err := blebridge.Read(os.Stdin)
	if err != nil || typ != blebridge.TypeOpen {
		return fmt.Errorf("first frame %q: %v", typ, err)
	}
	if mode == "fail" {
		return blebridge.Write(os.Stdout, blebridge.TypeError, []byte("no Bluetooth adapter: bluetooth is not authorized for this app"))
	}
	if err := blebridge.Write(os.Stdout, blebridge.TypeNotify, p); err != nil {
		return err
	}
	ok, _ := json.Marshal(blebridge.OK{MTU: 182})
	if err := blebridge.Write(os.Stdout, blebridge.TypeOK, ok); err != nil {
		return err
	}
	for {
		typ, p, err := blebridge.Read(os.Stdin)
		if err != nil {
			return err
		}
		switch typ {
		case blebridge.TypeClose:
			return nil
		case blebridge.TypeWrite:
			if mode == "die" {
				os.Exit(3)
			}
			if err := blebridge.Write(os.Stdout, blebridge.TypeNotify, p); err != nil {
				return err
			}
			if err := blebridge.Write(os.Stdout, blebridge.TypeOK, nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected frame %q", typ)
		}
	}
}

// useFake puts the fake helper on PATH (or, with mode "", an empty PATH).
func useFake(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	if mode != "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(dir, helperName)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv(fakeEnv, mode)
}

func newBridge(t *testing.T, c config.Transport) *Transport {
	t.Helper()
	c.Kind = "ble"
	m := &devicedb.Model{Quirks: map[string]any{"ble_gap_ms": 25}}
	tr, err := transport.New(c, m)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := tr.(*Transport)
	if !ok {
		t.Fatalf("kind ble is %T", tr)
	}
	return b
}

func readAll(t *testing.T, tr *Transport) string {
	t.Helper()
	p := make([]byte, 4096)
	n, err := tr.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(p[:n])
}

func TestBridgeTransport(t *testing.T) {
	useFake(t, "ok")
	tr := newBridge(t, config.Transport{Name: "MX10"})
	if tr.Kind() != "ble" {
		t.Errorf("Kind %q", tr.Kind())
	}
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	var req blebridge.OpenRequest
	if err := json.Unmarshal([]byte(readAll(t, tr)), &req); err != nil {
		t.Fatal(err)
	}
	want := blebridge.OpenRequest{Name: "MX10", Service: 0xae30, Write: 0xae01, Notify: 0xae02, GapMS: 25}
	if req != want {
		t.Errorf("open request %+v, want %+v", req, want)
	}
	if n, err := tr.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write: %d %v", n, err)
	}
	if got := readAll(t, tr); got != "hello" {
		t.Errorf("notification %q", got)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := tr.Write([]byte("x")); err == nil {
		t.Error("Write after Close succeeded")
	}
	// Opening again starts a fresh helper.
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	readAll(t, tr)
	if err := tr.Close(); err != nil {
		t.Error(err)
	}
}

func TestBridgeDisconnectMidWrite(t *testing.T) {
	useFake(t, "die")
	tr := newBridge(t, config.Transport{Address: "5E3F0A6B-0000-4000-8000-000000000001"})
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	readAll(t, tr) // the echoed open request
	if _, err := tr.Write([]byte("hello")); err == nil {
		t.Fatal("Write to a dead helper succeeded")
	}
	start := time.Now()
	if _, err := tr.Read(make([]byte, 10)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Read after the helper died: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("Read waited for the deadline instead of failing at once")
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if tr.cmd != nil {
		t.Error("Close left the helper process behind")
	}
}

func TestBridgeOpenErrors(t *testing.T) {
	tests := []struct {
		mode, want  string
		noBluetooth bool
	}{
		{"", "install pdb-print-ble (comes with the macOS release) for Bluetooth printers", false},
		{"fail", "not authorized", true},
	}
	for _, tt := range tests {
		t.Run(cmp.Or(tt.mode, "no helper"), func(t *testing.T) {
			useFake(t, tt.mode)
			tr := newBridge(t, config.Transport{Name: "MX10"})
			err := tr.Open(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Open: %v, want %q", err, tt.want)
			}
			if errors.Is(err, blecore.ErrNoBluetooth) != tt.noBluetooth {
				t.Errorf("Open: %v, ErrNoBluetooth want %v", err, tt.noBluetooth)
			}
			if tr.cmd != nil {
				t.Error("failed Open left the helper process behind")
			}
			if _, err := tr.Write([]byte("x")); err == nil {
				t.Error("Write after a failed Open succeeded")
			}
		})
	}
}

func TestBridgeScan(t *testing.T) {
	tests := []struct {
		mode        string
		ads         int
		noBluetooth bool
		missing     bool
	}{
		{mode: "scan", ads: 2},
		{mode: "scan-nobt", noBluetooth: true},
		{mode: "", missing: true},
	}
	for _, tt := range tests {
		t.Run(cmp.Or(tt.mode, "no helper"), func(t *testing.T) {
			useFake(t, tt.mode)
			ads, err := Scan(context.Background(), 8*time.Second)
			switch {
			case tt.missing:
				if err == nil || !strings.Contains(err.Error(), "install pdb-print-ble") {
					t.Errorf("Scan: %v", err)
				}
			case tt.noBluetooth:
				if !errors.Is(err, blecore.ErrNoBluetooth) {
					t.Errorf("Scan: %v, want ErrNoBluetooth", err)
				}
			default:
				if err != nil || len(ads) != tt.ads {
					t.Fatalf("Scan: %v %v", ads, err)
				}
				if a := ads[0]; a.Name != "MX10" || a.RSSI != -60 || len(a.Services) != 1 || a.Address == "" {
					t.Errorf("advert %+v", a)
				}
			}
		})
	}
}
