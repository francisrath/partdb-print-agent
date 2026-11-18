// Package report collects what we need to support a printer we don't have: discovery data,
// read-only status, and – only after the user confirms – identify queries and a test print.
// The result is one JSON file, redacted by default, to attach to a GitHub issue.
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

type Report struct {
	Tool       string                `json:"tool"`
	OS         string                `json:"os"`
	Time       string                `json:"time"`
	Candidates []discovery.Candidate `json:"candidates"`
	Queries    []Query               `json:"queries"`
	Notes      []string              `json:"notes,omitempty"`
}

// Query is one exchange with a printer. Sent and Reply are escaped text (non-ASCII as \xNN).
type Query struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	Sent   string `json:"sent,omitempty"`
	Reply  string `json:"reply,omitempty"`
	TookMS int64  `json:"took_ms"`
	Err    string `json:"error,omitempty"`
}

type Options struct {
	Version   string
	DB        *devicedb.DB
	Targets   []string // hosts or IPs; "usb" for USB only; none = everything discovery finds
	Port      string   // print port; "" = 9100
	NoUSB     bool
	NoNetwork bool
	Identify  bool // send identify queries to printers we can't name (asks first, per printer)
	Print     bool // print a test label on supported printers (asks first, per printer)
	Confirm   func(question string) bool
	Out       io.Writer // progress and summary
	Svc       *printing.Service
	Sysfs     string                                                 // "" = /sys
	SysDescr  func(ctx context.Context, host string) (string, error) // nil = SNMP
}

// identify queries per printer language; sent one per connection.
var identify = []struct{ name, data string }{
	{"zpl ~HI", "~HI\r\n"},
	{"zpl ~HS", "~HS\r\n"},
	{"brother status", string(make([]byte, 100)) + "\x1b@\x1biS"},
}

func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Port == "" {
		o.Port = "9100"
	}
	if o.Confirm == nil {
		o.Confirm = func(string) bool { return false }
	}
	r := &Report{Tool: "pdb-print " + o.Version, OS: runtime.GOOS + "/" + runtime.GOARCH, Time: time.Now().UTC().Format(time.RFC3339)}
	var hosts []netip.Prefix
	usbOnly := false
	for _, t := range o.Targets {
		if t == "usb" {
			usbOnly = true
			continue
		}
		a, err := netip.ParseAddr(t)
		if err != nil {
			ips, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip4", t)
			if lerr != nil || len(ips) == 0 {
				return nil, fmt.Errorf("target %s: not an address or a known host", t)
			}
			a = ips[0]
		}
		if a = a.Unmap(); !a.Is4() {
			return nil, fmt.Errorf("target %s: only IPv4 printers can be reported for now", t)
		}
		hosts = append(hosts, netip.PrefixFrom(a, 32))
	}
	if !o.NoUSB && (usbOnly || len(hosts) == 0) {
		cs, err := discovery.USB(o.DB, o.Sysfs)
		if err != nil {
			r.Notes = append(r.Notes, "usb: "+err.Error())
		}
		r.Candidates = append(r.Candidates, cs...)
	}
	if !o.NoNetwork && (len(hosts) > 0 || !usbOnly) {
		port, _ := strconv.Atoi(o.Port)
		cs, err := discovery.Network(ctx, o.DB, discovery.Options{Subnets: hosts, Port: port, SysDescr: o.SysDescr})
		if err != nil {
			r.Notes = append(r.Notes, "network: "+err.Error())
		}
		r.Candidates = append(r.Candidates, cs...)
	}
	for i, c := range r.Candidates {
		where := c.Device
		if c.Kind == "tcp" {
			where = net.JoinHostPort(c.Host, o.Port)
			r.query(where, "snmp printer-mib", "", func() (string, error) {
				st, err := snmp.Query(ctx, c.Host, "public", time.Second)
				return fmt.Sprintf("%+v", st), err
			})
		}
		if c.Status == discovery.Supported && c.Kind == "usb" {
			r.query(where, "driver status", "", func() (string, error) {
				d, err := driver.New(c.Model, driver.Options{})
				if err != nil {
					return "", err
				}
				t := usb.New(c.Device)
				if err := t.Open(ctx); err != nil {
					return "", err
				}
				defer t.Close()
				st, err := d.Status(ctx, t)
				return fmt.Sprintf("%+v media=%+v", st, mediaOf(st)), err
			})
		}
		if o.Identify && c.Status != discovery.Supported &&
			o.Confirm(fmt.Sprintf("Send identify queries to %s %s? A printer that doesn't understand them may print them as a page.", c.Kind, where)) {
			for _, q := range identify {
				r.exchange(ctx, c, where, q.name, q.data)
			}
		}
		if o.Print && o.Svc != nil && c.Status == discovery.Supported &&
			o.Confirm(fmt.Sprintf("Print a test label on the %s at %s?", c.ModelID, where)) {
			p := &config.Printer{ID: fmt.Sprintf("report-%d", i), Model: c.ModelID,
				Transport: config.Transport{Kind: c.Kind, Host: c.Host, Port: c.Port, Device: c.Device}}
			r.query(where, "test print", "", func() (string, error) {
				res, err := o.Svc.Print(ctx, p, "", printing.TestCard(c.ModelID, o.Version))
				return fmt.Sprintf("%+v", res), err
			})
		}
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "%d printer(s), %d queries\n", len(r.Candidates), len(r.Queries))
	}
	return r, nil
}

func mediaOf(st *driver.Status) any {
	if st == nil {
		return nil
	}
	return st.Media
}

func (r *Report) query(target, name, sent string, f func() (string, error)) {
	start := time.Now()
	reply, err := f()
	q := Query{Target: target, Name: name, Sent: escape([]byte(sent)), Reply: reply, TookMS: time.Since(start).Milliseconds()}
	if err != nil {
		q.Err = err.Error()
	}
	r.Queries = append(r.Queries, q)
}

// exchange sends data on a fresh connection (or the USB device) and records the reply.
func (r *Report) exchange(ctx context.Context, c discovery.Candidate, where, name, data string) {
	r.query(where, name, data, func() (string, error) {
		var t driver.Transport
		if c.Kind == "usb" {
			t = usb.New(c.Device)
		} else {
			conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", where)
			if err != nil {
				return "", err
			}
			t = &connTransport{conn}
		}
		if err := t.Open(ctx); err != nil {
			return "", err
		}
		defer t.Close()
		if _, err := t.Write([]byte(data)); err != nil {
			return "", err
		}
		var got []byte
		buf := make([]byte, 4096)
		for len(got) < 64<<10 {
			n, err := t.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil || n == 0 {
				break
			}
		}
		return escape(got), nil
	})
}

// connTransport reads with a 1.5 s idle timeout; replies end when the printer stops talking.
type connTransport struct{ net.Conn }

func (c *connTransport) Open(context.Context) error { return nil }
func (c *connTransport) Kind() string               { return "tcp" }
func (c *connTransport) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	return c.Conn.Read(p)
}

func escape(b []byte) string {
	q := strconv.QuoteToASCII(string(b))
	return q[1 : len(q)-1]
}

var (
	ipv4Re = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Re = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b|(?i)\b(?:[0-9a-f]{1,4}:)+:(?:[0-9a-f]{1,4}:?)*\b`)
	macRe  = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?:[:-][0-9a-f]{2}){5}\b`)
	// MAC without separators, e.g. Brother's default node name BRW008092A1B2C3
	bareMACRe = regexp.MustCompile(`(?i)\b(BR[WN])?[0-9a-f]{12}\b`)
)

// JSON renders the report; unless raw, IPv4 addresses (host-N, the same token for the same
// address), MAC addresses, USB serial numbers and this computer's name are masked.
func (r *Report) JSON(raw bool) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil || raw {
		return b, err
	}
	s := string(b)
	n := 0
	for _, c := range r.Candidates {
		if c.Serial != "" && strings.Contains(s, c.Serial) {
			n++
			s = strings.ReplaceAll(s, c.Serial, fmt.Sprintf("serial-%d", n))
		}
	}
	if h, _ := os.Hostname(); len(h) > 2 {
		s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(h)).ReplaceAllString(s, "this-computer")
	}
	s = macRe.ReplaceAllString(s, "xx:xx:xx:xx:xx:xx")
	s = bareMACRe.ReplaceAllString(s, "${1}xxxxxxxxxxxx")
	s = ipv6Re.ReplaceAllString(s, "ipv6-address")
	tokens := map[string]string{}
	s = ipv4Re.ReplaceAllStringFunc(s, func(ip string) string {
		if tokens[ip] == "" {
			tokens[ip] = fmt.Sprintf("host-%d", len(tokens)+1)
		}
		return tokens[ip]
	})
	return []byte(s), nil
}
