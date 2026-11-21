package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/server"
	"github.com/francisrath/partdb-print-agent/internal/transport/bt"
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
