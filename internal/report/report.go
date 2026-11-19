// Package report collects what we need to support a printer we don't have: discovery data,
// read-only status, and – only after the user confirms – identify queries and a test print.
// The result is one JSON file, redacted by default, to attach to a GitHub issue.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"slices"
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
	// Secrets are values found in replies that JSON masks like serial numbers (a Zebra's serial
	// and printer name).
	Secrets []string `json:"-"`
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

// Identify queries, one per connection: ~HI first; the other ZPL queries only to a printer that
// answered it (anything else could print them), the Brother status query only to one that didn't.
var (
	zplQueries = []struct{ name, data string }{
		{"zpl ~HS", "~HS\r\n"},
		{"zpl ~HQES", "~HQES\r\n"},
		{"zpl ~HQSN", "~HQSN\r\n"},
		{"zpl ^HH", "^XA^HH^XZ\r\n"},
		{"zpl ^HZA", "^XA^HZA^XZ\r\n"},
	}
	brotherQuery = string(make([]byte, 100)) + "\x1b@\x1biS"
)

// zebraSecrets finds a Zebra's serial number and printer name in a (escaped) reply.
var zebraSecretRes = []*regexp.Regexp{
	regexp.MustCompile(`([A-Z0-9]{6,})\s+SERIAL NUMBER`),                                     // ^HH
	regexp.MustCompile(`SERIAL NUMBER(?:\\r|\\n|\s)+([A-Z0-9]{6,})`),                         // ~HQSN
	regexp.MustCompile(`<PRINTER-NAME>([^<]+)</PRINTER-NAME>`),                               // ^HZA
	regexp.MustCompile(`<(?:SERIAL-NUMBER|HOST-NAME)>([^<]+)</(?:SERIAL-NUMBER|HOST-NAME)>`), // ^HZA, newer firmware
}

// ZebraQueries records a Zebra's read-only status and setup queries (~HS, ~HQES, ~HQSN, ^HH,
// ^HZA) and adds the serial number and printer name they reveal to the mask list. Only for a
// printer that answered ~HI.
func (r *Report) ZebraQueries(ctx context.Context, c discovery.Candidate, where string) {
	for _, q := range zplQueries {
		r.Secrets = append(r.Secrets, zebraSecrets(r.exchange(ctx, c, where, q.name, q.data))...)
	}
}

// Escape is how replies are recorded: non-ASCII as \xNN.
func Escape(b []byte) string { return escape(b) }

func zebraSecrets(reply string) []string {
	var out []string
	for _, re := range zebraSecretRes {
		for _, m := range re.FindAllStringSubmatch(reply, -1) {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
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
			if hi := r.exchange(ctx, c, where, "zpl ~HI", "~HI\r\n"); strings.Count(hi, ",") >= 2 {
				if m, ok := o.DB.ModelByHI(hi); ok && c.Kind == "tcp" {
					r.Candidates[i].Model, r.Candidates[i].ModelID = m, m.ID
					if slices.Contains(driver.Families(), m.Driver) {
						r.Candidates[i].Status = discovery.Supported
					}
				}
				r.ZebraQueries(ctx, c, where)
			} else {
				r.exchange(ctx, c, where, "brother status", brotherQuery)
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
// exchange returns the reply as recorded (escaped), "" when there was none.
func (r *Report) exchange(ctx context.Context, c discovery.Candidate, where, name, data string) string {
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
	return r.Queries[len(r.Queries)-1].Reply
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

// serialKeys are IEEE 1284 device ID keys that carry a serial number.
var serialKeys = map[string]bool{"SN": true, "SER": true, "SERN": true, "SERIAL": true, "SERIALNUMBER": true}

// JSON renders the report; unless raw, IPv4/IPv6 addresses (host-N, the same token for the
// same address), MAC addresses, serial numbers (all but the last 4 characters of long ones)
// and this computer's name are masked. Serial
// fields are masked whatever their length; inside free text (replies) a serial is replaced
// as a whole word from 3 characters on – shorter values would hit unrelated text.
func (r *Report) JSON(raw bool) ([]byte, error) {
	if raw {
		return json.MarshalIndent(r, "", "  ")
	}
	// serial → its mask: long serials keep their last 4 characters (the part that differs
	// between units, so the same printer is recognisable across reports without revealing
	// the serial); short ones, and a second serial ending alike, become serial-N
	tokens := map[string]string{}
	used := map[string]bool{}
	token := func(v string) string {
		if v == "" {
			return ""
		}
		if tokens[v] == "" {
			t := fmt.Sprintf("serial-%d", len(tokens)+1)
			if len(v) >= 8 {
				if tail := strings.Repeat("x", len(v)-4) + v[len(v)-4:]; !used[tail] {
					t = tail
				}
			}
			tokens[v], used[t] = t, true
		}
		return tokens[v]
	}
	for _, v := range r.Secrets {
		token(v)
	}
	masked := *r
	masked.Candidates = make([]discovery.Candidate, len(r.Candidates))
	for i, c := range r.Candidates {
		c.Serial = token(c.Serial)
		if c.IEEE1284 != nil {
			m := make(map[string]string, len(c.IEEE1284))
			for _, k := range slices.Sorted(maps.Keys(c.IEEE1284)) {
				m[k] = c.IEEE1284[k]
				if serialKeys[k] {
					m[k] = token(c.IEEE1284[k])
				}
			}
			c.IEEE1284 = m
		}
		masked.Candidates[i] = c
	}
	// serials inside free text, as whole words from 3 characters on (shorter ones would hit
	// unrelated text); done on the strings, not the JSON, so numbers elsewhere stay intact
	var res []*regexp.Regexp
	var toks []string
	for _, v := range slices.Sorted(maps.Keys(tokens)) {
		if len(v) >= 3 {
			res = append(res, regexp.MustCompile(`\b`+regexp.QuoteMeta(v)+`\b`))
			toks = append(toks, tokens[v])
		}
	}
	text := func(s string) string {
		for i, re := range res {
			s = re.ReplaceAllString(s, toks[i])
		}
		return s
	}
	masked.Queries = make([]Query, len(r.Queries))
	for i, q := range r.Queries {
		q.Target, q.Sent, q.Reply, q.Err = text(q.Target), text(q.Sent), text(q.Reply), text(q.Err)
		masked.Queries[i] = q
	}
	masked.Notes = make([]string, len(r.Notes))
	for i, n := range r.Notes {
		masked.Notes[i] = text(n)
	}
	for i := range masked.Candidates {
		c := &masked.Candidates[i]
		c.SNMPDescr = text(c.SNMPDescr)
		for k, v := range c.IEEE1284 {
			c.IEEE1284[k] = text(v)
		}
	}
	// no HTML escaping: "\u003e192.168.1.5" has no word boundary before the address, so the
	// masks below would miss addresses in XML replies
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&masked); err != nil {
		return nil, err
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	if h, _ := os.Hostname(); len(h) > 2 {
		s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(h)).ReplaceAllString(s, "this-computer")
	}
	s = macRe.ReplaceAllString(s, "xx:xx:xx:xx:xx:xx")
	s = bareMACRe.ReplaceAllString(s, "${1}xxxxxxxxxxxx")
	s = ipv6Re.ReplaceAllString(s, "ipv6-address")
	hosts := map[string]string{}
	s = ipv4Re.ReplaceAllStringFunc(s, func(ip string) string {
		if hosts[ip] == "" {
			hosts[ip] = fmt.Sprintf("host-%d", len(hosts)+1)
		}
		return hosts[ip]
	})
	return []byte(s), nil
}
