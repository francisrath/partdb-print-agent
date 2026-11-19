package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
)

func TestRedact(t *testing.T) {
	host, _ := os.Hostname()
	r := &Report{
		Candidates: []discovery.Candidate{{Kind: "tcp", Host: "192.168.1.40", Port: 9100}, {Kind: "usb", Device: "/dev/usb/lp0", Serial: "000G5G313834"},
			{Kind: "usb", Device: "/dev/usb/lp1", IEEE1284: map[string]string{"MDL": "X", "SN": "K7Q22Z9"}}},
		Queries: []Query{{Target: "192.168.1.40:9100", Name: "~HI", Reply: "IP 192.168.1.40 MAC aa:bb:cc:dd:ee:ff on " + host + " serial 000G5G313834"}},
	}
	b, err := r.JSON(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"192.168.1.40", "aa:bb:cc:dd:ee:ff", "000G5G313834", host, strings.ToUpper(host), "008092A1B2C3", "fe80::1ff:fe23:4567:890a", "K7Q22Z9"} {
		if secret != "" && bytes.Contains(bytes.ToLower(b), bytes.ToLower([]byte(secret))) {
			t.Errorf("%q not masked:\n%s", secret, b)
		}
	}
	if bytes.Count(b, []byte("host-1")) != 3 {
		t.Errorf("one address should become the same token everywhere:\n%s", b)
	}
	raw, _ := r.JSON(true)
	if !bytes.Contains(raw, []byte("192.168.1.40")) {
		t.Error("--raw masked the report")
	}
}

// fakeZebra answers ~HI like the GK420d did and records everything it receives.
// lockedBuf is a buffer written by the fake printer's goroutine and read by the test.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) Len() int       { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Len() }
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func fakeZebra(t *testing.T) (port string, got *lockedBuf) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = &lockedBuf{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf)
			got.Write(buf[:n])
			if bytes.Contains(buf[:n], []byte("~HI")) {
				_, _ = c.Write([]byte("\x02GK420d-200dpi,V61.17.11Z,8,2104KB\x03\r\n"))
			}
			c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, got
}

func options(port string, identify bool, confirm func(string) bool) Options {
	return Options{
		DB: devicedb.Default(), Targets: []string{"127.0.0.1"}, Port: port, Identify: identify, Confirm: confirm,
		Out: &bytes.Buffer{}, NoUSB: true,
		SysDescr: func(context.Context, string) (string, error) { return "", errors.New("no snmp") },
	}
}

// Identify queries can print a page on a printer that doesn't understand them: ask first.
func TestIdentifyAsksFirst(t *testing.T) {
	port, got := fakeZebra(t)
	asked := false
	r, err := Run(context.Background(), options(port, true, func(q string) bool { asked = true; return false }))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !asked || got.Len() != 0 || len(r.Candidates) != 1 {
		t.Errorf("asked=%v, printer received %q, %d candidates", asked, got.String(), len(r.Candidates))
	}
}

func TestIdentifyZebra(t *testing.T) {
	port, _ := fakeZebra(t)
	r, err := Run(context.Background(), options(port, true, func(string) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	var hi *Query
	for i := range r.Queries {
		if r.Queries[i].Name == "zpl ~HI" {
			hi = &r.Queries[i]
		}
	}
	if hi == nil || !strings.Contains(hi.Reply, "GK420d-200dpi") {
		t.Errorf("queries %+v", r.Queries)
	}
}

func TestReportRejectsIPv6Target(t *testing.T) {
	start := time.Now()
	o := options("9100", false, nil)
	o.Targets = []string{"::1"}
	if _, err := Run(context.Background(), o); err == nil || time.Since(start) > time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}

// Serial fields are masked whatever their length; free-text replies whole-word from 3 characters.
func TestRedactShortSerialFields(t *testing.T) {
	r := &Report{Candidates: []discovery.Candidate{{Kind: "usb", Device: "/dev/usb/lp0", Serial: "Q", IEEE1284: map[string]string{"MDL": "X", "SN": "AB"}}},
		Queries: []Query{{Target: "x", Name: "q", Reply: "ABC took 1 ms, sn K7Q22Z9X", TookMS: 1}}}
	r.Candidates = append(r.Candidates, discovery.Candidate{Kind: "usb", Device: "/dev/usb/lp1", IEEE1284: map[string]string{"SERN": "K7Q22Z9X"}})
	b, err := r.JSON(false)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("redacted report isn't valid JSON: %v", err)
	}
	if c := back.Candidates[0]; c.Serial == "Q" || c.IEEE1284["SN"] == "AB" {
		t.Errorf("short serial fields not masked: %+v", c)
	}
	if q := back.Queries[0]; q.Reply != "ABC took 1 ms, sn xxxx2Z9X" || q.TookMS != 1 {
		t.Errorf("free text: %+v", q)
	}
}

// A numeric serial must not replace numbers elsewhere (e.g. a port): masking works on the
// report's text before it is serialised.
func TestRedactNumericSerial(t *testing.T) {
	r := &Report{Candidates: []discovery.Candidate{{Kind: "usb", Device: "/dev/usb/lp0", Serial: "9100"}, {Kind: "tcp", Host: "10.0.0.9", Port: 9100}},
		Queries: []Query{{Target: "10.0.0.9:9100", Name: "q", Reply: "serial 9100"}}}
	b, err := r.JSON(false)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	if back.Candidates[1].Port != 9100 || back.Candidates[0].Serial == "9100" || back.Queries[0].Reply != "serial serial-1" {
		t.Errorf("%+v %+v", back.Candidates, back.Queries)
	}
}

// Long serials keep their last 4 characters (recognisable across reports, not revealing);
// short ones become serial-N; two serials ending alike stay distinguishable.
func TestRedactSerialKeepsLast4(t *testing.T) {
	r := &Report{Candidates: []discovery.Candidate{
		{Kind: "usb", Device: "/dev/usb/lp0", Serial: "D7G751568"},
		{Kind: "usb", Device: "/dev/usb/lp1", Serial: "Q8X221568"},
		{Kind: "usb", Device: "/dev/usb/lp2", Serial: "AB12"},
	}, Queries: []Query{{Target: "x", Name: "q", Reply: "sn D7G751568"}}}
	b, _ := r.JSON(false)
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got := []string{back.Candidates[0].Serial, back.Candidates[1].Serial, back.Candidates[2].Serial}
	if got[0] != "xxxxx1568" || got[1] == got[0] || strings.Contains(got[1], "Q8X2") || got[2] != "serial-3" || back.Queries[0].Reply != "sn xxxxx1568" {
		t.Errorf("serials %q, reply %q", got, back.Queries[0].Reply)
	}
}

func zebraOptions(t *testing.T, addr string) Options {
	_, port, _ := net.SplitHostPort(addr)
	return options(port, true, func(string) bool { return true })
}

// After a silent ~HI no ZPL follows: it isn't a Zebra (or one set to EPL only).
func TestReportStopsAfterSilentHI(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var got []byte
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
					got = append(got, b[:n]...)
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	if _, err := Run(context.Background(), zebraOptions(t, ln.Addr().String())); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if s := string(got); !strings.HasPrefix(s, "~HI\r\n") || strings.Contains(s, "~HS") || strings.Contains(s, "^XA") || strings.Contains(s, "~HQ") {
		t.Errorf("printer received %q", s)
	}
}

// A Zebra's serial number and printer name don't leave in the report.
func TestReportMasksZebraSerial(t *testing.T) {
	fake := &zpltest.Fake{}
	r, err := Run(context.Background(), zebraOptions(t, fake.Start(t)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := r.JSON(true)
	masked, _ := r.JSON(false)
	if !strings.Contains(string(raw), "28J124202270") || !strings.Contains(string(raw), "ZBR4580776") {
		t.Fatalf("queries didn't fetch serial and name: %s", raw)
	}
	if strings.Contains(string(masked), "28J124202270") || strings.Contains(string(masked), "ZBR4580776") {
		t.Errorf("serial or printer name left in the report:\n%s", masked)
	}
}
