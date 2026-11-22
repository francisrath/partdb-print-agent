package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/blecore"
	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/discovery"
	"github.com/francisrath/pdb-print/internal/logfile"
	"github.com/francisrath/pdb-print/internal/printing"
	"github.com/francisrath/pdb-print/internal/server"
	"github.com/francisrath/pdb-print/internal/transport/bt"
	"github.com/francisrath/pdb-print/internal/wininstall"
	"github.com/zalando/go-keyring"
)

// TestMain isolates the CLI tests from the developer's machine: no env from a loaded
// .env, no real OS keychain. Tests only ever print to fake printers on localhost.
func TestMain(m *testing.M) {
	for _, v := range []string{"PDB_PRINT_TOKEN", "PDB_PRINT_URL", "PDB_PRINT_CONFIG", "PDB_PRINT_ALLOW_HTTP"} {
		os.Unsetenv(v)
	}
	keyring.MockInit()
	os.Exit(m.Run())
}

func status(statusType byte, err2 byte) []byte {
	b := make([]byte, 32)
	b[0], b[1], b[2], b[3], b[4] = 0x80, 0x20, 'B', '4', 0x39 // QL-810W
	b[9], b[10], b[11], b[18] = err2, 29, 0x0a, statusType    // 29 mm continuous
	return b
}

// fakeQL answers the status request, then reports the page printed. It never touches hardware.
func fakeQL(t *testing.T, coverOpen bool) (addr string, got chan []byte) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var rx bytes.Buffer
		buf := make([]byte, 4096)
		replied := false
		for {
			n, err := c.Read(buf)
			rx.Write(buf[:n])
			if !replied && bytes.Contains(rx.Bytes(), []byte{0x1b, 'i', 'S'}) {
				replied = true
				if coverOpen {
					c.Write(status(0x02, 0x10))
				} else {
					c.Write(status(0x00, 0))
				}
			}
			if bytes.HasSuffix(rx.Bytes(), []byte{0x1a}) {
				c.Write(status(0x01, 0))
			}
			if err != nil {
				got <- rx.Bytes()
				return
			}
		}
	}()
	return ln.Addr().String(), got
}

func writeConfig(t *testing.T, addr string, extra ...string) string {
	host, port, _ := net.SplitHostPort(addr)
	p := filepath.Join(t.TempDir(), "config.yaml")
	cfg := strings.Join(extra, "") + fmt.Sprintf("printers:\n  - id: fake\n    model: QL-810W\n    transport: {kind: tcp, host: %s, port: %s}\n    media: dk-22210\n    no_snmp: true\n", host, port)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrintToFakePrinter(t *testing.T) {
	addr, got := fakeQL(t, false)
	cfg := writeConfig(t, addr)
	if err := run(context.Background(), []string{"--config", cfg, "print", "../../testdata/sample-29x46.pdf", "--printer", "fake"}); err != nil {
		t.Fatal(err)
	}
	rx := <-got
	// status request, then the job: 485 raster lines for a 29×46 page on 29 mm tape
	if !bytes.Contains(rx, []byte{0x1b, 'i', 'z', 0xce, 0x0a, 29, 0, 0xe5, 0x01, 0, 0}) {
		t.Errorf("print info for 485 lines missing")
	}
	if c := bytes.Count(rx, []byte{'g', 0}); c < 485 {
		t.Errorf("%d raster lines", c)
	}
}

func TestPrintRefusesWhenCoverOpen(t *testing.T) {
	addr, _ := fakeQL(t, true)
	cfg := writeConfig(t, addr)
	err := run(context.Background(), []string{"--config", cfg, "print", "../../testdata/sample-29x46.pdf", "--printer", "fake"})
	if err == nil || !strings.Contains(err.Error(), "COVER_OPEN") {
		t.Errorf("err = %v", err)
	}
}

func TestDryRunLandscape(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")
	out := filepath.Join(t.TempDir(), "job.bin")
	if err := run(context.Background(), []string{"--config", cfg, "print", "--dry-run", out, "--printer", "fake", "../../testdata/sample-50x29.pdf"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !bytes.HasSuffix(b, []byte{0x1a}) || bytes.Count(b, []byte{'g', 0}) < 528 {
		t.Errorf("unexpected dry-run output (%d bytes)", len(b))
	}
}

func TestPrintFromPartDB(t *testing.T) {
	sample, err := os.ReadFile("../../testdata/sample-29x46.pdf")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody string
	pdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/labels/generate" || r.Header.Get("Authorization") != "Bearer tcp_test" {
			http.Error(w, `{"detail":"nope"}`, http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(sample)
	}))
	defer pdb.Close()

	addr, got := fakeQL(t, false)
	// no --printer: the route for the (only) instance "home" and profile 2 picks it
	cfg := writeConfig(t, addr, "instances:\n  - id: home\n    base_url: "+pdb.URL+"\nroutes:\n  - instance: home\n    profile: 2\n    printer: fake\n")
	t.Setenv("PDB_PRINT_TOKEN", "tcp_test")
	err = run(context.Background(), []string{"--config", cfg, "print", "--profile", "2", "--ids", "32", "--type", "part"})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"elementIds":"32","elementType":"part","profileId":2}` {
		t.Errorf("request body %s", gotBody)
	}
	if rx := <-got; !bytes.HasSuffix(rx, []byte{0x1a}) {
		t.Error("no job reached the printer")
	}

	t.Setenv("PDB_PRINT_TOKEN", "tcp_wrong")
	err = run(context.Background(), []string{"--config", cfg, "print", "--profile", "2", "--ids", "32", "--printer", "fake"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}
}

func TestPrintArgs(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")
	for _, args := range [][]string{
		{"print", "--printer", "fake"},
		{"print", "x.pdf", "--profile", "2", "--ids", "1", "--printer", "fake"},
		{"print", "--profile", "2", "--printer", "fake"},
		{"print", "--profile", "2", "--ids", "1", "--type", "lot", "--printer", "fake"},
	} {
		if err := run(context.Background(), append([]string{"--config", cfg}, args...)); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestPrintersTestDryRun(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")
	out := filepath.Join(t.TempDir(), "test.bin")
	if err := run(context.Background(), []string{"--config", cfg, "printers", "test", "fake", "--dry-run", out}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.HasSuffix(b, []byte{0x1a}) {
		t.Error("no job written")
	}
}

func TestServeLogsWhichPartDBItFetchesJobsFrom(t *testing.T) {
	partDB := httptest.NewServer(http.NotFoundHandler()) // no agent API: the loop just waits
	t.Cleanup(partDB.Close)
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfgText := fmt.Sprintf("instances:\n  - id: home\n    base_url: %s\n    token: tcp_test\n  - id: work\n    base_url: https://partdb.example.org\n", partDB.URL)
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a := &app{cfg: cfg, cfgPath: path, log: slog.New(slog.NewTextHandler(&logs, nil))}
	srv := server.New(cfg, path, nil, a.log, "127.0.0.1:0", "test")

	ctx, cancel := context.WithCancel(context.Background())
	jobs := a.startAgentJobs(ctx, srv)
	cancel()
	jobs.wait()

	out := logs.String()
	if !strings.Contains(out, `msg="fetching print jobs" instance=home url=`+partDB.URL) {
		t.Errorf("no startup line for the instance with a token:\n%s", out)
	}
	if !strings.Contains(out, `msg="not fetching print jobs" instance=work`) || !strings.Contains(out, "API token") {
		t.Errorf("no line saying why the instance without a token is skipped:\n%s", out)
	}
}

func TestServeWithoutPartDBSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	a := &app{cfg: cfg, cfgPath: path, log: slog.New(slog.NewTextHandler(&logs, nil))}
	a.startAgentJobs(context.Background(), server.New(cfg, path, nil, a.log, "127.0.0.1:0", "test")).wait()

	if out := logs.String(); !strings.Contains(out, `msg="not fetching print jobs" reason="no Part-DB connected"`) {
		t.Errorf("want a plain reason when no Part-DB is set up:\n%s", out)
	}
}

// `pdb-print media` is what the config errors point to, so it must work with a broken config.
func TestMediaRunsWithBrokenConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(cfg, []byte("printers: [{id: a, model: QL-810W, media: dk-22201, transport: {kind: tcp, host: x}}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--config", cfg, "media"}); err != nil {
		t.Errorf("media with a broken config: %v", err)
	}
}

// discover needs no config and never writes to the printers it finds.
func TestDiscoverWithoutConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wrote := make(chan bool, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		n, _ := c.Read(make([]byte, 1))
		wrote <- n > 0
		c.Close()
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg := filepath.Join(t.TempDir(), "none.yaml")
	if err := run(context.Background(), []string{"--config", cfg, "discover", "--no-usb", "--no-ble", "--subnet", "127.0.0.1/32", "--port", port}); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-wrote:
		if w {
			t.Error("discover wrote to the printer port")
		}
	case <-time.After(2 * time.Second):
		t.Error("discover didn't connect")
	}
}

// No Bluetooth adapter is a note: the network printers are still listed.
func TestDiscoverNoAdapter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	noAdapter := func(context.Context, time.Duration) ([]blecore.Advert, error) {
		return nil, fmt.Errorf("%w: adaptor is not powered", blecore.ErrNoBluetooth)
	}
	noClassic := func(context.Context, time.Duration) ([]bt.Device, error) {
		return nil, fmt.Errorf("bt: %w: adaptor is not powered", blecore.ErrNoBluetooth)
	}
	var out bytes.Buffer
	if err := discover(context.Background(), []string{"--no-usb", "--subnet", "127.0.0.1/32", "--port", port}, &out,
		discovery.Options{ScanBLE: noAdapter, ScanBT: noClassic}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "tcp  127.0.0.1:"+port) || strings.Count(got, "Bluetooth printers not searched") != 1 {
		t.Errorf("output:\n%s", got)
	}
}

// A Bluetooth Classic printer is listed with its config line.
func TestDiscoverBT(t *testing.T) {
	noBLE := func(context.Context, time.Duration) ([]blecore.Advert, error) { return nil, nil }
	rego := func(context.Context, time.Duration) ([]bt.Device, error) {
		return []bt.Device{{Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:01"}, {Name: "Phone", Address: "AA:BB:CC:DD:EE:02", Class: 0x5a020c}}, nil
	}
	var out bytes.Buffer
	if err := discover(context.Background(), []string{"--no-usb", "--no-network"}, &out, discovery.Options{ScanBLE: noBLE, ScanBT: rego}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"bt   AA:BB:CC:DD:EE:01", `transport: {kind: bt, name: "RG-MLP80B", address: "AA:BB:CC:DD:EE:01"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Phone") || strings.Contains(got, "not searched") {
		t.Errorf("output:\n%s", got)
	}
}

// A bad flag returns at once, not after the Bluetooth scan.
func TestDiscoverBadSubnetFast(t *testing.T) {
	slow := func(ctx context.Context, _ time.Duration) ([]blecore.Advert, error) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return nil, nil
	}
	slowBT := func(ctx context.Context, _ time.Duration) ([]bt.Device, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	err := discover(context.Background(), []string{"--no-usb", "--subnet", "nope"}, io.Discard, discovery.Options{ScanBLE: slow, ScanBT: slowBT})
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}

// report can't reach a Bluetooth device: no report hint for an unknown one.
func TestPrintCandidatesUnknownBluetooth(t *testing.T) {
	for _, c := range []discovery.Candidate{
		{Kind: "ble", Address: "AA:BB:CC:DD:EE:03", BLEName: "XY99", Status: discovery.Unknown},
		{Kind: "bt", Address: "AA:BB:CC:DD:EE:04", BTName: "XY99", Status: discovery.Unknown},
	} {
		var b bytes.Buffer
		printCandidates(&b, []discovery.Candidate{c})
		if out := b.String(); strings.Contains(out, "pdb-print report") || !strings.Contains(out, "XY99") || !strings.Contains(out, c.Address) {
			t.Errorf("%s output:\n%s", c.Kind, out)
		}
	}
}

func TestConfirmStopsOnCancel(t *testing.T) {
	r, w := io.Pipe() // nothing is ever typed
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if confirm(ctx, bufio.NewReader(r), "print?") || time.Since(start) > time.Second {
		t.Errorf("confirm didn't give up on cancel (%v)", time.Since(start))
	}
	if !confirm(context.Background(), bufio.NewReader(strings.NewReader("y\n")), "print?") {
		t.Error("y not accepted")
	}
}

// `pdb-print pair` asks the running service to open the setup window and shows where to go.
func TestPairCmd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/pair" || r.Header.Get("Origin") != "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"code":"482913","urls":["http://pi.local:8631/setup/","http://192.168.1.23:8631/setup/"],"expires":"2026-11-19T12:15:00Z"}`)
	}))
	t.Cleanup(srv.Close)
	var out bytes.Buffer
	if err := pairCmd(context.Background(), []string{"--addr", strings.TrimPrefix(srv.URL, "http://")}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"482913", "http://pi.local:8631/setup/", "http://192.168.1.23:8631/setup/"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := pairCmd(context.Background(), []string{"--addr", "127.0.0.1:1"}, &out); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("no service: %v", err)
	}
	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"482913","urls":[],"expires":"2026-11-19T12:15:00Z"}`)
	}))
	t.Cleanup(none.Close)
	if err := pairCmd(context.Background(), []string{"--addr", strings.TrimPrefix(none.URL, "http://")}, &out); err == nil || !strings.Contains(err.Error(), "network") {
		t.Errorf("no LAN address bound: %v", err)
	}
}

func TestPairCmdReadsCodeFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pair-code.txt")
	if err := os.WriteFile(file, []byte("482913"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code_file":%q,"urls":[],"expires":"2026-11-19T12:15:00Z"}`, file)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	var out bytes.Buffer
	if err := pairCmd(context.Background(), []string{"--addr", addr}, &out); err != nil {
		t.Fatal(err) // no LAN address is fine: this PC's browser can pair
	}
	for _, want := range []string{"482913", "http://" + addr + "/setup/"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(file); err == nil {
		t.Skip("running as root/administrator: can't test an unreadable file")
	}
	if err := pairCmd(context.Background(), []string{"--addr", addr}, &out); err == nil || !strings.Contains(err.Error(), "as administrator") {
		t.Errorf("unreadable code file: %v", err)
	}
}

// On Windows, pair proves admin rights to the service: a fresh nonce in %ProgramData%\pdb-print\pair-request,
// sent along. Without the folder (no rights, no service) it asks without one.
func TestPairCmdSendsRequestNonce(t *testing.T) {
	pd := t.TempDir()
	stub(t, &goos, "windows")
	stub(t, &envFromOS, func() wininstall.Env { return wininstall.Env{ProgramData: pd} })
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(server.PairRequestHeader))
		fmt.Fprint(w, `{"code":"482913","urls":["http://192.168.1.23:8631/setup/"],"expires":"2026-11-19T12:15:00Z"}`)
	}))
	t.Cleanup(srv.Close)
	addr := []string{"--addr", strings.TrimPrefix(srv.URL, "http://")}
	var out bytes.Buffer
	if err := pairCmd(context.Background(), addr, &out); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(pd, "pdb-print"), 0o700)
	for range 2 {
		if err := pairCmd(context.Background(), addr, &out); err != nil {
			t.Fatal(err)
		}
	}
	file, _ := os.ReadFile(filepath.Join(pd, "pdb-print", "pair-request"))
	if len(got) != 3 || got[0] != "" || len(got[1]) != 64 || got[2] == got[1] || got[2] != string(file) {
		t.Errorf("nonces sent %q, file %q", got, file)
	}
}

func TestNeedsSetup(t *testing.T) {
	t.Setenv("PDB_PRINT_URL", "")
	if !needsSetup(&config.Config{}) {
		t.Error("fresh install should open the setup window")
	}
	if needsSetup(&config.Config{Instances: []config.Instance{{ID: "default"}}}) {
		t.Error("configured instance")
	}
	t.Setenv("PDB_PRINT_URL", "https://partdb.example.org")
	if needsSetup(&config.Config{}) {
		t.Error("instance from the environment")
	}
}

// When the agent id couldn't be saved at startup, connecting Part-DB later still starts the jobs.
func TestJobsStartAfterAgentIDRecovers(t *testing.T) {
	partDB := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(partDB.Close)
	cfg := &config.Config{}
	var logs syncBuffer
	a := &app{cfg: cfg, log: slog.New(slog.NewTextHandler(&logs, nil))}
	srv := server.New(cfg, t.TempDir(), nil, a.log, "127.0.0.1:0", "test") // a directory: saving fails
	ctx, cancel := context.WithCancel(context.Background())
	jobs := a.startAgentJobs(ctx, srv)
	if srv.OnInstance == nil {
		t.Fatal("no hook after the agent id failed")
	}
	srv.CfgPath = filepath.Join(t.TempDir(), "config.yaml") // storage is back
	srv.OnInstance(config.Instance{ID: "default", BaseURL: partDB.URL, Token: "tcp_test"})
	cancel()
	jobs.wait()
	if !strings.Contains(logs.String(), `msg="fetching print jobs" instance=default`) {
		t.Errorf("jobs not started:\n%s", logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the loop goroutines' logging.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestTryArgs(t *testing.T) {
	if err := run(context.Background(), []string{"try", "10.0.0.1", "10.0.0.2"}); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("two addresses: %v", err)
	}
}

// One block per printer, with a blank line between, so long names don't run together.
func TestPrintCandidates(t *testing.T) {
	var b bytes.Buffer
	printCandidates(&b, []discovery.Candidate{
		{Kind: "tcp", Host: "192.168.1.160", Port: 9100, ModelID: "QL-810W", Status: discovery.Supported},
		{Kind: "tcp", Host: "192.168.1.252", Port: 9100, SNMPDescr: "Brother HL-L3270CDW series; Brother NC-9400h, Firmware Ver.Z", Status: discovery.Unknown},
	})
	out := b.String()
	if !strings.Contains(out, "transport: {kind: tcp, host: 192.168.1.160}\n\ntcp  192.168.1.252:9100") {
		t.Errorf("no blank line between printers:\n%s", out)
	}
	if strings.Contains(out, "NC-9400h") || !strings.Contains(out, "Brother HL-L3270CDW series") {
		t.Errorf("long SNMP description not cut:\n%s", out)
	}
}

func TestWhere(t *testing.T) {
	for _, c := range []struct {
		t    config.Transport
		want string
	}{
		{config.Transport{Kind: "tcp", Host: "printer.lan", Port: 9100}, "printer.lan:9100"},
		{config.Transport{Kind: "usb", USBID: "04f9:209c", Serial: "X1"}, "usb 04f9:209c X1"},
		{config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}, "/dev/usb/lp0"},
		{config.Transport{Kind: "serial", Device: "COM3"}, "COM3"},
		{config.Transport{Kind: "ble", Name: "MX10"}, "Bluetooth MX10"},
		{config.Transport{Kind: "bt", Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:FF"}, "Bluetooth AA:BB:CC:DD:EE:FF"},
	} {
		if got := where(c.t); got != c.want {
			t.Errorf("%+v: %q, want %q", c.t, got, c.want)
		}
	}
}

// fakeSys is a wininstall.System in memory: no registry, services or processes.
type fakeSys struct {
	admin, service, portBusy bool
	spawns                   int
	unin                     map[wininstall.Mode]wininstall.Entry
	waitErr                  error // WaitExit's result; nil = the process exits and frees the port
	waited                   []int // WaitExit's pids
	waitTimeout              time.Duration
	readErr                  error // ReadUninstall's error
	stops                    int   // StopService calls
}

func (f *fakeSys) IsAdmin() bool                { return f.admin }
func (f *fakeSys) SetRun(string, string) error  { return nil }
func (f *fakeSys) DeleteRun(string) error       { return nil }
func (f *fakeSys) ServiceExists() (bool, error) { return f.service, nil }
func (f *fakeSys) InstallService(string, []string) error {
	f.service = true
	return nil
}
func (f *fakeSys) RemoveService() error               { f.service = false; return nil }
func (f *fakeSys) StartService() error                { f.portBusy = true; return nil }
func (f *fakeSys) StopService(time.Duration) error    { f.stops++; f.portBusy = false; return nil }
func (f *fakeSys) PortInUse(string) bool              { return f.portBusy }
func (f *fakeSys) Listening(string) bool              { return f.portBusy }
func (f *fakeSys) DeleteAfterExit(string) error       { return nil }
func (f *fakeSys) OwnedByAdmins(string) (bool, error) { return true, nil }
func (f *fakeSys) Spawn(string, ...string) error      { f.spawns++; f.portBusy = true; return nil }
func (f *fakeSys) WaitExit(pid int, timeout time.Duration) error {
	f.waited, f.waitTimeout = append(f.waited, pid), timeout
	if f.waitErr == nil {
		f.portBusy = false
	}
	return f.waitErr
}
func (f *fakeSys) WriteUninstall(m wininstall.Mode, e wininstall.Entry) error {
	f.unin[m] = e
	return nil
}
func (f *fakeSys) ReadUninstall(m wininstall.Mode) (wininstall.Entry, bool, error) {
	if f.readErr != nil {
		return wininstall.Entry{}, false, f.readErr
	}
	e, ok := f.unin[m]
	return e, ok, nil
}
func (f *fakeSys) DeleteUninstall(m wininstall.Mode) error { delete(f.unin, m); return nil }

const fakePID = 4242

// fakeWin records what the install commands did; see fakeWindows.
type fakeWin struct {
	sys     *fakeSys
	env     wininstall.Env
	quits   int
	browsed []string
}

// fakeWindows makes the install commands run as on Windows against fakeSys, in temp folders.
// The quitAgent stub answers fakePID and counts quits; opened browser URLs go to browsed.
func fakeWindows(t *testing.T) *fakeWin {
	t.Helper()
	dir := t.TempDir()
	e := wininstall.Env{LocalAppData: filepath.Join(dir, "local"), AppData: filepath.Join(dir, "roaming"),
		ProgramFiles: filepath.Join(dir, "pf"), ProgramData: filepath.Join(dir, "pd")}
	e.StartMenuUser, e.StartMenuAll = filepath.Join(e.AppData, "start"), filepath.Join(e.ProgramData, "start")
	src := filepath.Join(dir, "Downloads")
	os.MkdirAll(src, 0o755)
	for _, n := range []string{"pdb-print.exe", "pdb-printw.exe"} {
		os.WriteFile(filepath.Join(src, n), []byte("exe"), 0o755)
	}
	w := &fakeWin{sys: &fakeSys{unin: map[wininstall.Mode]wininstall.Entry{}}, env: e}
	stub(t, &goos, "windows")
	stub(t, &newSystem, func() wininstall.System { return w.sys })
	stub(t, &envFromOS, func() wininstall.Env { return e })
	stub(t, &selfExe, func() (string, error) { return filepath.Join(src, "pdb-print.exe"), nil })
	stub(t, &quitAgent, func(context.Context) (int, error) { w.quits++; return fakePID, nil })
	stub(t, &openBrowser, func(u string) error { w.browsed = append(w.browsed, u); return nil })
	stub(t, &startedByExplorer, func() bool { return false })
	stub(t, &portPoll, time.Millisecond)
	stub(t, &startWait, 5*time.Millisecond)
	return w
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// stub sets *p to v for the test.
func stub[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func runOut(args ...string) (string, error) {
	var out bytes.Buffer
	old := stdout
	stdout = &out
	defer func() { stdout = old }()
	err := run(context.Background(), args)
	return out.String(), err
}

func TestInstallCmdNonWindows(t *testing.T) {
	fakeWindows(t)
	goos = "linux"
	for _, cmd := range []string{"install", "uninstall"} {
		if err := run(context.Background(), []string{cmd}); err == nil || !strings.Contains(err.Error(), "only on Windows") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
}

func TestInstallService(t *testing.T) {
	w := fakeWindows(t)
	if err := run(context.Background(), []string{"install", "--service"}); err == nil || !strings.Contains(err.Error(), "run it as administrator") {
		t.Errorf("not admin: %v", err)
	}
	if _, err := runOut("install", "--quiet"); err != nil { // a per-user install, running
		t.Fatal(err)
	}
	// it is stopped (and waited for) and replaced by the service
	w.sys.admin = true
	out, err := runOut("install", "--service")
	if err != nil {
		t.Fatal(err)
	}
	l := wininstall.LayoutFor(wininstall.Service, w.env)
	if _, ok := w.sys.unin[wininstall.PerUser]; ok || !w.sys.service || w.quits != 1 || len(w.sys.waited) != 1 || w.sys.stops != 0 {
		t.Errorf("takeover: entries %v, service %v, %d quits, waited %v, %d stops", w.sys.unin, w.sys.service, w.quits, w.sys.waited, w.sys.stops)
	}
	for _, want := range []string{"runs as the Windows service pdb-print", l.ProgramDir, "pdb-print pair", "Replaced the per-user install"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(w.browsed) != 1 {
		t.Errorf("browsed %v", w.browsed)
	}
	// again: the running service is stopped by the service manager, not /v1/quit (it has none)
	if _, err := runOut("install", "--service", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if w.quits != 1 || w.sys.stops != 1 || !w.sys.service {
		t.Errorf("reinstall: %d quits, %d stops", w.quits, w.sys.stops)
	}
	// uninstall without admin (Settings → Apps): it runs itself again elevated, with the same args
	w.sys.admin = false
	for _, tt := range []struct {
		code int
		err  error
		want string // in the error; "" = none
	}{{0, nil, ""}, {3, nil, "exit status 3"}, {0, errUACDeclined, "declined"}} {
		var elevated [][]string
		var cwd string
		t.Chdir(l.ProgramDir) // Settings → Apps starts the uninstall in it, and the elevated one removes it
		stub(t, &runElevated, func(args []string) (int, error) {
			elevated, cwd = append(elevated, args), must(os.Getwd())
			return tt.code, tt.err
		})
		_, err := runOut("uninstall", "--purge")
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("code %d, %v: err %v", tt.code, tt.err, err)
		}
		if len(elevated) != 1 || !slices.Equal(elevated[0], []string{"uninstall", "--service", "--purge"}) {
			t.Errorf("elevated %v", elevated)
		}
		if rel, _ := filepath.Rel(l.ProgramDir, cwd); !strings.HasPrefix(rel, "..") {
			t.Errorf("relaunched from inside the program dir: %s", cwd)
		}
		var ec exitCode
		if tt.code != 0 && (!errors.As(err, &ec) || ec != 3) {
			t.Errorf("exit code not passed: %v", err)
		}
	}
	if !w.sys.service {
		t.Fatal("the unelevated uninstall removed the service itself")
	}
	w.sys.admin = true
	if _, err := runOut("uninstall"); err != nil {
		t.Fatal(err)
	}
	if w.quits != 1 || w.sys.stops != 2 || w.sys.service || len(w.sys.unin) != 0 {
		t.Errorf("uninstall: %d quits, %d stops, service %v, entries %v", w.quits, w.sys.stops, w.sys.service, w.sys.unin)
	}
}

// With both installed, uninstall removes the install its exe belongs to (Settings → Apps runs that
// entry's exe), not the per-user one first.
func TestUninstallPicksOwnInstall(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	ls, lu := wininstall.LayoutFor(wininstall.Service, w.env), wininstall.LayoutFor(wininstall.PerUser, w.env)
	w.sys.unin[wininstall.Service] = wininstall.Entry{InstallLocation: ls.ProgramDir}
	w.sys.service = true
	var elevated [][]string
	stub(t, &runElevated, func(args []string) (int, error) { elevated = append(elevated, args); return 0, nil })
	stub(t, &selfExe, func() (string, error) { return ls.Exe, nil }) // the service entry's uninstall
	if _, err := runOut("uninstall"); err != nil {
		t.Fatal(err)
	}
	if len(elevated) != 1 || !slices.Equal(elevated[0], []string{"uninstall", "--service"}) {
		t.Errorf("elevated %v", elevated)
	}
	if _, ok := w.sys.unin[wininstall.PerUser]; !ok {
		t.Error("the per-user install was removed")
	}
	// the elevated child: --service even when run from elsewhere
	w.sys.admin = true
	stub(t, &selfExe, func() (string, error) { return lu.Exe, nil })
	if _, err := runOut("uninstall", "--service"); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.sys.unin[wininstall.PerUser]; !ok || w.sys.service {
		t.Errorf("--service: entries %v, service %v", w.sys.unin, w.sys.service)
	}
	// the per-user entry's exe removes the per-user install
	w.sys.unin[wininstall.Service], w.sys.service = wininstall.Entry{InstallLocation: ls.ProgramDir}, true
	if _, err := runOut("uninstall"); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.sys.unin[wininstall.PerUser]; ok || !w.sys.service {
		t.Errorf("per-user: entries %v, service %v", w.sys.unin, w.sys.service)
	}
}

func TestServiceCmd(t *testing.T) {
	for _, args := range [][]string{{"service"}, {"service", "start"}, {"service", "run"}} {
		if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "usage: pdb-print service run --config FILE") {
			t.Errorf("%v: %v", args, err)
		}
	}
	// not started by the service manager (here: not Windows)
	if err := run(context.Background(), []string{"service", "run", "--config", "x.yaml"}); err == nil || !strings.Contains(err.Error(), "Windows service") {
		t.Errorf("by hand: %v", err)
	}
}

// As the service: pairing on loopback, its files next to the config, no /v1/quit (the service manager stops it).
func TestNewServerServiceMode(t *testing.T) {
	dir := t.TempDir()
	a := &app{cfg: &config.Config{}, cfgPath: filepath.Join(dir, "config.yaml"), log: slog.New(slog.DiscardHandler), svc: &printing.Service{}}
	stub(t, &goos, "windows")
	quit := func() {}
	if s := a.newServer("127.0.0.1:8631", quit); s.Quit == nil || s.PairLoopback || s.PairCodeFile != "" || s.SessionKeyFile != "" {
		t.Errorf("per-user: %+v", s)
	}
	stub(t, &serviceMode, true)
	s := a.newServer("127.0.0.1:8631", quit)
	if s.Quit != nil || !s.PairLoopback || s.PairCodeFile != filepath.Join(dir, "pair-code.txt") || s.SessionKeyFile != filepath.Join(dir, "session.key") ||
		s.PairRequestFile != filepath.Join(dir, "pair-request") {
		t.Errorf("service: quit %v, loopback %v, %q %q", s.Quit != nil, s.PairLoopback, s.PairCodeFile, s.SessionKeyFile)
	}
}

// The service's exit: a failure restarts it (service-specific code), but not after the admin stopped it.
func TestServiceExit(t *testing.T) {
	boom := errors.New("boom")
	for _, tt := range []struct {
		err      error
		stopping bool
		ssec     bool
		code     uint32
		logged   bool
	}{{nil, false, false, 0, false}, {nil, true, false, 0, false}, {boom, false, true, 1, false}, {boom, true, false, 0, true}} {
		var log bytes.Buffer
		ssec, code := serviceExit(tt.err, tt.stopping, &log)
		if ssec != tt.ssec || code != tt.code || (log.Len() > 0) != tt.logged {
			t.Errorf("%v stopping=%v: %v %d log %q", tt.err, tt.stopping, ssec, code, log.String())
		}
	}
}

// A process WaitExit may not open (another user's, an elevated one): wait for the port instead.
func TestStopAgentCantWatch(t *testing.T) {
	w := fakeWindows(t)
	stub(t, &startWait, 0) // the fallback must use the long limit
	n := 50
	w.sys.portBusy, w.sys.waitErr = true, wininstall.ErrCantWatch
	if err := stopAgent(context.Background(), freesAfter{w.sys, &n}); err != nil || n > 0 {
		t.Errorf("err %v, polls left %d", err, n)
	}
}

func TestInstallPortBusy(t *testing.T) {
	w := fakeWindows(t)
	w.sys.portBusy = true
	err := run(context.Background(), []string{"install"})
	if err == nil || !strings.Contains(err.Error(), "port 8631") || !strings.Contains(err.Error(), "another pdb-print") {
		t.Errorf("err = %v", err)
	}
	if w.quits != 0 || w.sys.spawns != 0 {
		t.Errorf("not installed, yet quit %d, spawned %d", w.quits, w.sys.spawns)
	}
}

func TestInstallRefusedWhenServiceInstalled(t *testing.T) {
	w := fakeWindows(t)
	w.sys.service = true
	if err := run(context.Background(), []string{"install"}); err == nil || !strings.Contains(err.Error(), "installed as a service") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	l := wininstall.LayoutFor(wininstall.PerUser, w.env)
	if _, err := os.Stat(l.ExeW); err != nil || w.sys.spawns != 1 || w.quits != 0 {
		t.Fatalf("install: %v, %d spawns, %d quits", err, w.sys.spawns, w.quits)
	}
	// again, with the agent running: it is stopped first and waited for until it has exited
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if w.sys.spawns != 2 || w.quits != 1 || !slices.Equal(w.sys.waited, []int{fakePID}) || w.sys.waitTimeout != 2*time.Minute+10*time.Second {
		t.Errorf("reinstall: %d spawns, %d quits, waited for %v (%s)", w.sys.spawns, w.quits, w.sys.waited, w.sys.waitTimeout)
	}
	if _, err := runOut("uninstall"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.ProgramDir); !os.IsNotExist(err) || w.quits != 2 || len(w.sys.waited) != 2 || len(w.sys.unin) != 0 {
		t.Errorf("uninstall: %v, %d quits, waited %v, entries %v", err, w.quits, w.sys.waited, w.sys.unin)
	}
}

// An uninstall entry that can't be read fails the command instead of saying "not installed".
func TestUninstallUnreadableEntry(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	w.sys.readErr = errors.New("access denied")
	out, err := runOut("uninstall")
	if err == nil || strings.Contains(out, "not installed") {
		t.Errorf("%v:\n%s", err, out)
	}
}

func TestUninstallNotInstalled(t *testing.T) {
	w := fakeWindows(t)
	w.sys.portBusy = true // a copy running from a zip
	out, err := runOut("uninstall")
	if err != nil || out != "pdb-print is not installed.\n" || w.quits != 0 {
		t.Errorf("%v, %d quits:\n%s", err, w.quits, out)
	}
}

// Started from Settings → Apps: the window stays open until Enter.
// From Settings → Apps: a successful uninstall closes its window by itself after a moment; an
// error waits for Enter so it can be read.
func TestUninstallFromSettingsWindow(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	startedByExplorer = func() bool { return true }
	stub(t, &closeDelay, 0)
	stub[io.Reader](t, &stdin, strings.NewReader("\n"))
	w.sys.portBusy, w.sys.waitErr = true, errors.New("still there")
	out, err := runOut("uninstall")
	if err == nil || !strings.Contains(out, "error:") || !strings.HasSuffix(out, "Press Enter to close.") {
		t.Errorf("failed uninstall: %v:\n%s", err, out)
	}
	w.sys.portBusy, w.sys.waitErr = false, nil
	out, err = runOut("uninstall")
	if err != nil || !strings.Contains(out, "pdb-print is uninstalled.") || strings.Contains(out, "Press Enter") {
		t.Errorf("successful uninstall: %v:\n%s", err, out)
	}
}

func TestInstallAsAdminWarns(t *testing.T) {
	w := fakeWindows(t)
	t.Setenv("USERNAME", "jdoe")
	w.sys.admin = true
	out, err := runOut("install", "--quiet")
	if err != nil || !strings.HasPrefix(out, "Installing for jdoe (this window runs as administrator; the agent it starts runs as administrator until you log in again).\n") || w.sys.spawns != 1 {
		t.Errorf("%v, %d spawns:\n%s", err, w.sys.spawns, out)
	}
	w.sys.admin = false
	if out, _ := runOut("install", "--quiet"); strings.Contains(out, "administrator") {
		t.Errorf("warned without admin:\n%s", out)
	}
}

func TestLogWriter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	for _, tc := range []struct {
		goos, gui string
		file      bool
	}{{"windows", "1", true}, {"windows", "", false}, {"linux", "1", false}} {
		stub(t, &goos, tc.goos)
		stub(t, &gui, tc.gui)
		w := logWriter()
		lf, ok := w.(*logfile.Writer)
		if ok {
			lf.Close() // also resets the crash output
		}
		if ok != tc.file || (!ok && w != io.Writer(os.Stderr)) {
			t.Errorf("%s gui=%q: %T", tc.goos, tc.gui, w)
		}
		if ok && (lf.Path != filepath.Join(dir, "pdb-print", "pdb-print.log") || lf.Max != 10<<20) {
			t.Errorf("file %s, max %d", lf.Path, lf.Max)
		}
	}
}

func TestInstallAgentWontStop(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	w.sys.waitErr = errors.New("timeout")
	for _, cmd := range []string{"install", "uninstall"} {
		if _, err := runOut(cmd); err == nil || !strings.Contains(err.Error(), "was asked to stop but is still running") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if w.sys.spawns != 1 || len(w.sys.unin) != 1 {
		t.Errorf("went ahead anyway: %d spawns, entries %v", w.sys.spawns, w.sys.unin)
	}
}

// The installed agent isn't what holds 8631 (or is too old for /v1/quit).
func TestInstallQuitFails(t *testing.T) {
	fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	quitAgent = func(context.Context) (int, error) { return 0, errors.New("403 Forbidden") }
	if _, err := runOut("install"); err == nil || !strings.Contains(err.Error(), "port 8631 is in use and pdb-print couldn't stop it: 403") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallFailsAfterStop(t *testing.T) {
	fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil {
		t.Fatal(err)
	}
	selfExe = func() (string, error) { return filepath.Join(t.TempDir(), "pdb-print.exe"), nil } // no pdb-printw.exe there
	_, err := runOut("install", "--quiet")
	if err == nil || !strings.Contains(err.Error(), "the running pdb-print was stopped") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallOpensBrowser(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet"); err != nil || len(w.browsed) != 0 {
		t.Fatalf("--quiet: %v, opened %v", err, w.browsed)
	}
	if _, err := runOut("install"); err != nil || !slices.Equal(w.browsed, []string{"http://127.0.0.1:8631"}) {
		t.Errorf("%v, opened %v", err, w.browsed)
	}
}

func TestInstallAgentSlowToListen(t *testing.T) {
	w := fakeWindows(t)
	newSystem = func() wininstall.System { return slowSys{w.sys} }
	out, err := runOut("install")
	if err != nil || len(w.browsed) != 0 || !strings.Contains(out, "pdb-print is starting; open http://127.0.0.1:8631 in a moment.") {
		t.Errorf("%v, opened %v:\n%s", err, w.browsed, out)
	}
}

// slowSys: the spawned agent never gets to listen.
type slowSys struct{ *fakeSys }

func (s slowSys) Spawn(string, ...string) error { s.spawns++; return nil }

func TestDoubleClickPrompt(t *testing.T) {
	for _, tc := range []struct {
		in        string
		installed bool
	}{{"y\n", true}, {"\n", true}, {"YES\n\n", true}, {"n\n", false}, {"later\n", false}} {
		t.Run(strings.TrimSpace(tc.in), func(t *testing.T) {
			w := fakeWindows(t)
			startedByExplorer = func() bool { return true }
			stub[io.Reader](t, &stdin, strings.NewReader(tc.in))
			out, err := runOut()
			if err != nil {
				t.Fatal(err)
			}
			if got := w.sys.spawns == 1; got != tc.installed {
				t.Errorf("installed = %v:\n%s", got, out)
			}
			if !strings.HasSuffix(out, "Press Enter to close.") {
				t.Errorf("no Enter prompt at the end:\n%s", out)
			}
			if !tc.installed && !strings.Contains(out, "usage: pdb-print") {
				t.Errorf("no usage:\n%s", out)
			}
		})
	}
}

// The flag reaches wininstall.Install, which writes it (tested there).
func TestNoSelfUpdateWritesConfig(t *testing.T) {
	w := fakeWindows(t)
	if _, err := runOut("install", "--quiet", "--no-self-update"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(wininstall.LayoutFor(wininstall.PerUser, w.env).Config, true)
	if err != nil || cfg.SelfUpdate == nil || *cfg.SelfUpdate {
		t.Errorf("load: %v %v", cfg, err)
	}
}

func TestPostQuit(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		pid    int
		ok     bool
	}{
		{http.StatusAccepted, `{"pid":7}`, 7, true},
		{http.StatusAccepted, `{}`, 0, true}, // asked to quit all the same
		{http.StatusAccepted, ``, 0, true},
		{http.StatusForbidden, `forbidden`, 0, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.Header.Get("X-PDB-Print") != "1" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		stub(t, &quitURL, srv.URL+"/v1/quit")
		pid, err := postQuit(context.Background())
		if pid != tc.pid || (err == nil) != tc.ok {
			t.Errorf("%d %q: pid %d, err %v", tc.status, tc.body, pid, err)
		}
		srv.Close()
	}
}

// freesAfter frees the port after n more PortInUse calls.
type freesAfter struct {
	*fakeSys
	n *int
}

func (f freesAfter) PortInUse(string) bool {
	if *f.n--; *f.n <= 0 {
		f.portBusy = false
	}
	return f.portBusy
}

func TestStopAgentWithoutPID(t *testing.T) {
	w := fakeWindows(t)
	quitAgent = func(context.Context) (int, error) { return 0, nil }
	stub(t, &startWait, 0) // the fallback must use the long limit, not this one
	n := 50
	sys := freesAfter{w.sys, &n}
	w.sys.portBusy = true
	if err := stopAgent(context.Background(), sys); err != nil || len(w.sys.waited) != 0 {
		t.Errorf("err %v, WaitExit %v", err, w.sys.waited)
	}
	stub(t, &exitWait, 5*time.Millisecond)
	w.sys.portBusy, n = true, 1<<30
	err := stopAgent(context.Background(), sys)
	if err == nil || !strings.Contains(err.Error(), "was asked to stop but is still running; end it in Task Manager (pdb-printw.exe or pdb-print.exe)") {
		t.Errorf("never frees: %v", err)
	}
}
