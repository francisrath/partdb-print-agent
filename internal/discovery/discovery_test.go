package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	_ "github.com/francisrath/partdb-print-agent/internal/driver/brother"
	_ "github.com/francisrath/partdb-print-agent/internal/driver/zpl"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
)

func sysfs(t *testing.T, printers map[string][4]string) string {
	t.Helper()
	root := t.TempDir()
	for lp, p := range printers {
		dev := filepath.Join(root, "devices", lp)
		iface := filepath.Join(dev, "if")
		for _, d := range []string{iface, filepath.Join(root, "class", "usbmisc", lp)} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for f, v := range map[string]string{"idVendor": p[0], "idProduct": p[1], "serial": p[2]} {
			_ = os.WriteFile(filepath.Join(dev, f), []byte(v), 0o644)
		}
		_ = os.WriteFile(filepath.Join(iface, "ieee1284_id"), []byte(p[3]), 0o644)
		if err := os.Symlink(iface, filepath.Join(root, "class", "usbmisc", lp, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUSBCandidates(t *testing.T) {
	sys := sysfs(t, map[string][4]string{
		"lp0": {"04f9", "209c", "S1", "MFG:Brother;MDL:QL-810W;"},
		"lp1": {"1234", "0001", "", "MFG:Acme;MDL:LabelMaker 3000;"},
	})
	cs, err := USB(devicedb.Default(), sys)
	if err != nil || len(cs) != 2 {
		t.Fatalf("%+v %v", cs, err)
	}
	if cs[0].Model == nil || cs[0].Model.ID != "QL-810W" || cs[0].Status != Supported || cs[0].Serial != "S1" {
		t.Errorf("QL: %+v", cs[0])
	}
	if cs[1].Model != nil || cs[1].Status != Unknown || cs[1].IEEE1284["MDL"] != "LabelMaker 3000" {
		t.Errorf("unknown: %+v", cs[1])
	}
	if got := TransportLine(cs[0]); got != `transport: {kind: usb, usb_id: "04f9:209c", serial: "S1"}` {
		t.Errorf("transport line %q", got)
	}
}

// noWrite is a port-9100 connection discovery must never write to.
type noWrite struct {
	net.Conn
	t *testing.T
}

func (c noWrite) Write([]byte) (int, error) {
	c.t.Error("discovery wrote to a printer")
	return 0, errors.New("no")
}
func (c noWrite) Close() error { return nil }

func TestNetwork(t *testing.T) {
	open := map[string]bool{"10.1.2.5:9100": true, "10.1.2.9:9100": true}
	opts := Options{
		Subnets: []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24")},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			if open[addr] {
				return noWrite{t: t}, nil
			}
			return nil, errors.New("refused")
		},
		SysDescr: func(ctx context.Context, host string) (string, error) {
			if host == "10.1.2.5" {
				return "Brother QL-810W", nil
			}
			return "", errors.New("timeout")
		},
	}
	cs, err := Network(context.Background(), devicedb.Default(), opts)
	if err != nil || len(cs) != 2 {
		t.Fatalf("%+v %v", cs, err)
	}
	if cs[0].Host != "10.1.2.5" || cs[0].Model == nil || cs[0].Model.ID != "QL-810W" || cs[0].Status != Supported {
		t.Errorf("QL: %+v", cs[0])
	}
	if cs[1].Host != "10.1.2.9" || cs[1].Status != Unknown {
		t.Errorf("silent printer: %+v", cs[1])
	}
	if got := TransportLine(cs[0]); got != `transport: {kind: tcp, host: 10.1.2.5}` {
		t.Errorf("transport line %q", got)
	}
}

func TestSubnetsCapped(t *testing.T) {
	got := capTo24([]netip.Prefix{netip.MustParsePrefix("172.16.5.20/16"), netip.MustParsePrefix("192.168.1.7/24"), netip.MustParsePrefix("10.0.0.9/30")})
	want := []netip.Prefix{netip.MustParsePrefix("172.16.5.0/24"), netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("10.0.0.8/30")}
	if !slices.Equal(got, want) {
		t.Errorf("%v", got)
	}
	start := time.Now()
	_, _ = Network(context.Background(), devicedb.Default(), Options{
		Subnets:     []netip.Prefix{netip.MustParsePrefix("10.9.0.0/24")},
		DialTimeout: 50 * time.Millisecond,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a /24 of silent hosts took %v", d)
	}
}

func TestStatusNoDriver(t *testing.T) {
	db, err := devicedb.Load(fstest.MapFS{"data/z.yaml": {Data: []byte("models:\n  - {id: ZZ-1, driver: zz, dialect: x, transports: [usb], match: {usb: [\"abcd:0001\"]}}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	sys := sysfs(t, map[string][4]string{"lp0": {"abcd", "0001", "", "MDL:ZZ-1;"}})
	cs, _ := USB(db, sys)
	if len(cs) != 1 || cs[0].Status != NoDriver {
		t.Errorf("%+v", cs)
	}
}

func TestNetworkRejectsIPv6AndHugeSubnets(t *testing.T) {
	for _, p := range []string{"fd00::/64", "10.0.0.0/8"} {
		start := time.Now()
		_, err := Network(context.Background(), devicedb.Default(), Options{Subnets: []netip.Prefix{netip.MustParsePrefix(p)},
			Dial: func(context.Context, string) (net.Conn, error) { return nil, errors.New("refused") }})
		if err == nil || time.Since(start) > time.Second {
			t.Errorf("%s: err %v after %v", p, err, time.Since(start))
		}
	}
}

func TestNetworkExplicitSubnetScannedWhole(t *testing.T) {
	var mu sync.Mutex
	n := 0
	_, err := Network(context.Background(), devicedb.Default(), Options{Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/23")},
		Dial: func(context.Context, string) (net.Conn, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return nil, errors.New("refused")
		}})
	if err != nil || n != 512 {
		t.Errorf("%d addresses, %v", n, err)
	}
}

// Ctrl-C (context cancel) or the overall time limit stops the scan.
func TestNetworkStops(t *testing.T) {
	block := func(ctx context.Context, _ string) (net.Conn, error) {
		time.Sleep(200 * time.Millisecond)
		return nil, errors.New("refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, _ = Network(ctx, devicedb.Default(), Options{Subnets: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/22")}, Workers: 4, Dial: block})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancel: still scanning after %v", d)
	}
	start = time.Now()
	_, _ = Network(context.Background(), devicedb.Default(), Options{Subnets: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/22")}, Workers: 4, Dial: block, Timeout: 300 * time.Millisecond})
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout: still scanning after %v", d)
	}
}

// silentPrinter accepts connections, records what it gets and never answers (a non-Zebra on 9100).
func silentPrinter(t *testing.T) (addr string, got func() string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var buf []byte
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					mu.Lock()
					buf = append(buf, b[:n]...)
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), func() string { mu.Lock(); defer mu.Unlock(); return string(buf) }
}

func candidateAt(t *testing.T, addr string) Candidate {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return Candidate{Kind: "tcp", Host: host, Port: p, Status: Unknown}
}

func TestIdentifyZebra(t *testing.T) {
	fake := &zpltest.Fake{}
	c, reply, err := Identify(context.Background(), devicedb.Default(), candidateAt(t, fake.Start(t)))
	if err != nil || c.ModelID != "GK420d" || c.Status != Supported || !strings.Contains(reply, "GK420d-200dpi") {
		t.Errorf("%+v %q %v", c, reply, err)
	}
}

func TestIdentifyNonZebra(t *testing.T) {
	addr, got := silentPrinter(t)
	c, _, err := Identify(context.Background(), devicedb.Default(), candidateAt(t, addr))
	if err == nil || c.Status != Unknown || !strings.Contains(err.Error(), "EPL") {
		t.Errorf("%+v %v", c, err)
	}
	time.Sleep(50 * time.Millisecond)
	if g := got(); g != "~HI\r\n" {
		t.Errorf("sent %q, want only ~HI", g)
	}
}
