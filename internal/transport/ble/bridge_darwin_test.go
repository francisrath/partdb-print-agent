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
	"slices"
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

const (
	fakeEnv  = "PDB_PRINT_BLE_FAKE"
	readyEnv = "PDB_PRINT_BLE_READY" // a file the fake creates once it handles an interrupt
)

// TestMain doubles as the fake pdb-print-ble: the tests point helperPath at a symlink to this test binary
// named pdb-print-ble and pick its behaviour through fakeEnv.
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
// "slowclose" takes the real helper's longest disconnect wait to close; "hang" never answers the open; "fail" refuses it; "closeonopen" answers it with a close frame; "scan" / "scan-nobt" / "scan-hang" answer the scan mode;
// "scan-enabling" is interrupted while waiting for Bluetooth; "abort" dies at once by SIGABRT, as when macOS privacy (TCC) kills it.
func fakeHelper(mode string, args []string) error {
	switch mode {
	case "abort": // a shell dies of the signal (Go would catch it and exit 2)
		return syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -ABRT $$"}, nil)
	case "scan-hang": // stuck in Enable, deaf to the interrupt
		signal.Ignore(os.Interrupt)
		time.Sleep(time.Hour)
	case "scan-enabling": // waiting for Bluetooth (a permission prompt) when interrupted
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		if err := os.WriteFile(os.Getenv(readyEnv), nil, 0o600); err != nil {
			return err
		}
		<-c
		return blecore.ErrNotEnabled
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
	case "scan-classic":
		if strings.Join(args, " ") != "scan-classic --timeout 8s" {
			return fmt.Errorf("args %q", args)
		}
		fmt.Println(`{"address":"AA:BB:CC:DD:EE:FF","name":"RG-MLP80B","class":1664}`)
		fmt.Println(`{"address":"11:22:33:44:55:66","name":"","class":0}`)
		return nil
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
	if mode == "bt-off" {
		return blebridge.Write(os.Stdout, blebridge.TypeError, []byte("RG-MLP80B (AA:BB:CC:DD:EE:FF) did not answer – is it switched on and paired with this computer?"))
	}
	if mode == "fail" {
		return blebridge.Write(os.Stdout, blebridge.TypeError, []byte("no Bluetooth adapter: bluetooth is not authorized for this app"))
	}
	if mode == "closeonopen" { // a mismatched helper: a well-formed frame that isn't an answer
		if err := blebridge.Write(os.Stdout, blebridge.TypeClose, nil); err != nil {
			return err
		}
		time.Sleep(time.Hour)
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
			if mode == "slowclose" { // the real helper's longest wait for notifications off and the disconnect
				time.Sleep(blecore.NotifyOffWait + blecore.DisconnectWait)
			}
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

// useFake makes helperPath find the fake helper (or, with mode "", no helper), never a real pdb-print-ble
// next to the test binary or on PATH.
func useFake(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, helperName)
	if mode != "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, helper); err != nil {
			t.Fatal(err)
		}
	}
	old := helperPath
	helperPath = func() (string, error) {
		if mode == "" {
			return "", errNoHelper
		}
		return helper, nil
	}
	t.Cleanup(func() { helperPath = old })
	t.Setenv(fakeEnv, mode)
	t.Setenv(readyEnv, filepath.Join(dir, "ready"))
	// a -race test binary sleeps 1 s at exit by default, which would eat into the close grace
	t.Setenv("GORACE", "atexit_sleep_ms=0")
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

// The helper waits for the disconnect before it exits (else macOS may keep the printer connected): Close
// gives it time for that instead of killing it.
func TestBridgeCloseWaitsForDisconnect(t *testing.T) {
	useFake(t, "slowclose")
	tr := newBridge(t, config.Transport{Name: "MX10"})
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	readAll(t, tr)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.deadErr(); err == nil || err.Error() != "ble: pdb-print-ble exited" {
		t.Errorf("helper ended with %v, want a clean exit (not killed)", err)
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
		{"closeonopen", "ble: pdb-print-ble: unexpected reply 'C'", false},
		{"abort", denied + " (run it from Terminal or another terminal app and allow Bluetooth when asked; a service needs its own permission); the helper exited: signal: abort trap", false},
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
				if err == nil || !strings.Contains(err.Error(), denied) || !strings.HasSuffix(err.Error(), "permission); the helper exited: signal: abort trap") ||
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

// The caller's deadline (the setup wizard gives discovery 30 s) ends the helper's wait for a first-run
// permission prompt: the helper's reason wins over the context's error.
func TestBridgeScanEndsWhileEnabling(t *testing.T) {
	useFake(t, "scan-enabling")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // cancel once the fake can take the interrupt; within below is the watchdog
		for {
			if _, err := os.Stat(os.Getenv(readyEnv)); err == nil || ctx.Err() != nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	err := within(t, 4*time.Second, "Scan", func() error {
		_, err := Scan(ctx, 8*time.Second)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "if macOS asked for permission, allow it") || errors.Is(err, blecore.ErrNoBluetooth) {
		t.Errorf("Scan: %v, want the not-enabled message", err)
	}
}

func newBT(t *testing.T, c config.Transport) *Transport {
	t.Helper()
	c.Kind = "bt"
	m, ok := devicedb.Default().ModelByBTName("RG-MLP80B")
	if !ok {
		t.Fatal("no escpos model for RG-MLP80B")
	}
	tr, err := transport.New(c, m)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := tr.(*Transport)
	if !ok {
		t.Fatalf("kind bt is %T", tr)
	}
	return b
}

// Kind bt goes through the same helper, as an RFCOMM link; the escpos driver needs Read's timeout to wrap
// os.ErrDeadlineExceeded and a dropped link to be a real error.
func TestBridgeRFCOMM(t *testing.T) {
	useFake(t, "ok")
	tr := newBT(t, config.Transport{Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:FF", Channel: 1})
	if tr.Kind() != "bt" {
		t.Errorf("Kind %q", tr.Kind())
	}
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	var req blebridge.OpenRequest
	if err := json.Unmarshal([]byte(readAll(t, tr)), &req); err != nil {
		t.Fatal(err)
	}
	want := blebridge.OpenRequest{Kind: "rfcomm", Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:FF", Channel: 1}
	if req != want {
		t.Errorf("open request %+v, want %+v", req, want)
	}
	if _, err := tr.Write([]byte{0x10, 0x04, 0x01}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, tr); got != "\x10\x04\x01" {
		t.Errorf("data %q", got)
	}
	if _, err := tr.Read(make([]byte, 10)); !errors.Is(err, os.ErrDeadlineExceeded) || !strings.HasPrefix(err.Error(), "bt: ") {
		t.Errorf("Read with nothing sent: %v, want bt: … os.ErrDeadlineExceeded", err)
	}
	if err := tr.Close(); err != nil {
		t.Error(err)
	}
}

func TestBridgeRFCOMMErrors(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		useFake(t, "bt-off")
		tr := newBT(t, config.Transport{Name: "RG-MLP80B"})
		err := tr.Open(context.Background())
		if want := "bt: RG-MLP80B (AA:BB:CC:DD:EE:FF) did not answer – is it switched on and paired with this computer?"; err == nil || err.Error() != want {
			t.Errorf("Open: %v, want %q", err, want)
		}
	})
	t.Run("no bluetooth", func(t *testing.T) {
		useFake(t, "fail")
		tr := newBT(t, config.Transport{Name: "RG-MLP80B"})
		if err := tr.Open(context.Background()); !errors.Is(err, blecore.ErrNoBluetooth) || !strings.HasPrefix(err.Error(), "bt: ") {
			t.Errorf("Open: %v, want bt: … ErrNoBluetooth", err)
		}
	})
	t.Run("link dropped", func(t *testing.T) {
		useFake(t, "drop")
		tr := newBT(t, config.Transport{Name: "RG-MLP80B"})
		if err := tr.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		readAll(t, tr)
		if _, err := tr.Write([]byte("x")); err == nil {
			t.Error("Write to a dropped link succeeded")
		}
		want := "bt: pdb-print-ble: printer 5E3F0A6B disconnected (exit status 1)"
		if _, err := tr.Read(make([]byte, 10)); err == nil || err.Error() != want {
			t.Errorf("Read after the link dropped: %v, want %q", err, want)
		}
		_ = tr.Close()
		if _, err := tr.Write([]byte("x")); err == nil || err.Error() != "bt: not open" {
			t.Errorf("Write after Close: %v, want bt: not open", err)
		}
	})
}

func TestScanClassic(t *testing.T) {
	useFake(t, "scan-classic")
	devs, err := ScanClassic(context.Background(), 8*time.Second)
	want := []ClassicDevice{{Address: "AA:BB:CC:DD:EE:FF", Name: "RG-MLP80B", Class: 1664}, {Address: "11:22:33:44:55:66"}}
	if err != nil || !slices.Equal(devs, want) {
		t.Errorf("ScanClassic: %+v %v, want %+v", devs, err, want)
	}
	useFake(t, "abort")
	if _, err := ScanClassic(context.Background(), 8*time.Second); err == nil || !strings.HasPrefix(err.Error(), "bt: "+denied) {
		t.Errorf("ScanClassic killed by privacy: %v", err)
	}
}
