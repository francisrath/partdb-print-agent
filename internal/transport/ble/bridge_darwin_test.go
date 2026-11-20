//go:build darwin

package ble

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
			fmt.Fprintln(os.Stderr, "error: "+err.Error()) // marked like the real helper's fatal line
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeHelper modes: "ok" echoes the open request and every write back as notifications; on the first
// write "die" exits silently, "drop" exits after logging a lost link, "half" logs it, writes half a frame
// and exits, "unmarked" exits after an ordinary log line, "junk" sends a bad frame and hangs;
// "hang" never answers the open; "fail" refuses it; "scan" / "scan-nobt" / "scan-hang" answer the scan mode;
// "abort" dies at once by SIGABRT, as when macOS privacy (TCC) kills it.
func fakeHelper(mode string, args []string) error {
	switch mode {
	case "abort": // a shell dies of the signal (Go would catch it and exit 2)
		return syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -ABRT $$"}, nil)
	case "scan-hang": // stuck in Enable, deaf to the interrupt
		signal.Ignore(os.Interrupt)
		time.Sleep(time.Hour)
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
	if mode == "hang" {
		time.Sleep(time.Hour)
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
			switch mode {
			case "die":
				os.Exit(3)
			case "drop":
				fmt.Fprintln(os.Stderr, "pdb-print-ble: connected to 5E3F0A6B")
				fmt.Fprintln(os.Stderr, "error: printer 5E3F0A6B disconnected")
				os.Exit(1)
			case "half":
				fmt.Fprintln(os.Stderr, "error: printer 5E3F0A6B disconnected")
				_, _ = os.Stdout.Write([]byte{'N', 0, 0, 0, 10, 'a', 'b'})
				os.Exit(1)
			case "unmarked":
				fmt.Fprintln(os.Stderr, "error: an earlier hiccup")
				fmt.Fprintln(os.Stderr, "pdb-print-ble: connected to 5E3F0A6B")
				os.Exit(1)
			case "junk":
				_, _ = os.Stdout.Write([]byte{'N', 0xff, 0xff, 0xff, 0xff})
				time.Sleep(time.Hour)
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

const denied = "macOS stopped pdb-print-ble from using Bluetooth because the app that started pdb-print isn't allowed to"

func TestBridgeOpenErrors(t *testing.T) {
	tests := []struct {
		mode, want  string
		noBluetooth bool
	}{
		{"", "install pdb-print-ble (comes with the macOS release) for Bluetooth printers", false},
		{"fail", "not authorized", true},
		{"abort", denied + " (run it from Terminal or another terminal app and allow Bluetooth when asked; a service needs its own permission) (signal: abort trap)", false},
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
		denied      bool
	}{
		{mode: "scan", ads: 2},
		{mode: "scan-nobt", noBluetooth: true},
		{mode: "abort", denied: true},
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
			case tt.denied:
				if err == nil || !strings.Contains(err.Error(), denied) || !strings.HasSuffix(err.Error(), "(signal: abort trap)") ||
					errors.Is(err, blecore.ErrNoBluetooth) {
					t.Errorf("Scan: %v, want %q and not ErrNoBluetooth", err, denied)
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

// within fails the test when f takes longer than d (a hang), and returns f's error otherwise.
func within(t *testing.T, d time.Duration, what string, f func() error) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- f() }()
	select {
	case err := <-errc:
		return err
	case <-time.After(d):
		t.Fatalf("%s still running after %v", what, d)
		return nil
	}
}

func TestBridgeHelperBreaks(t *testing.T) {
	tests := []struct{ mode, want string }{
		{"junk", "frame exceeds"},
		{"drop", "ble: pdb-print-ble: printer 5E3F0A6B disconnected (exit status 1)"},
		{"half", "ble: pdb-print-ble: printer 5E3F0A6B disconnected (exit status 1)"},
		// Only a marked fatal line that is the last line names the cause.
		{"unmarked", "ble: pdb-print-ble exited: exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			useFake(t, tt.mode)
			tr := newBridge(t, config.Transport{Name: "MX10"})
			if err := tr.Open(context.Background()); err != nil {
				t.Fatal(err)
			}
			readAll(t, tr)
			err := within(t, time.Second, "Write", func() error {
				_, err := tr.Write([]byte("hello"))
				return err
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Write: %v, want %q", err, tt.want)
			}
			if _, err := tr.Read(make([]byte, 10)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Read: %v, want %q", err, tt.want)
			}
			if err := within(t, time.Second, "Close", tr.Close); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestBridgeOpenCancel(t *testing.T) {
	useFake(t, "hang")
	tr := newBridge(t, config.Transport{Name: "MX10"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := within(t, time.Second, "Open", func() error { return tr.Open(ctx) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Open: %v", err)
	}
	if tr.cmd != nil {
		t.Error("cancelled Open left the helper process behind")
	}
}

func TestBridgeScanCancel(t *testing.T) {
	useFake(t, "scan-hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var ads []blecore.Advert
	err := within(t, 4*time.Second, "Scan", func() error {
		var err error
		ads, err = Scan(ctx, 8*time.Second)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) || ads != nil {
		t.Errorf("Scan: %v %v, want the context's error", ads, err)
	}
}
