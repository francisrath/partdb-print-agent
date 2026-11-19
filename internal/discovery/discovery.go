// Package discovery finds printers on USB (Linux sysfs) and the local network, and names them
// from the device database. It is passive: it never writes to a printer.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

// Candidate statuses.
const (
	Supported = "supported" // known model with a driver in this build
	NoDriver  = "no-driver" // known model, no driver yet
	Unknown   = "unknown"   // not in the device database (or didn't say what it is)
)

type Candidate struct {
	Kind      string            `json:"kind"` // usb | tcp
	Device    string            `json:"device,omitempty"`
	Host      string            `json:"host,omitempty"`
	Port      int               `json:"port,omitempty"`
	USBID     string            `json:"usb_id,omitempty"`
	Serial    string            `json:"serial,omitempty"`
	IEEE1284  map[string]string `json:"ieee1284,omitempty"`
	SNMPDescr string            `json:"snmp_descr,omitempty"`
	Model     *devicedb.Model   `json:"-"`
	ModelID   string            `json:"model,omitempty"`
	Status    string            `json:"status"`
}

func (c *Candidate) setModel(m *devicedb.Model) {
	c.Model, c.Status = m, Unknown
	if m == nil {
		return
	}
	c.ModelID, c.Status = m.ID, NoDriver
	if slices.Contains(driver.Families(), m.Driver) {
		c.Status = Supported
	}
}

// USB lists the usblp printers in sysfs ("" = /sys), named by USB id, then IEEE 1284 model.
func USB(db *devicedb.DB, sysfs string) ([]Candidate, error) {
	ps, err := usb.List(sysfs)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, p := range ps {
		c := Candidate{Kind: "usb", Device: p.Device, USBID: p.USBID, Serial: p.Serial, IEEE1284: p.IEEE1284}
		c.setModel(byUSB(db, p.USBID, p.IEEE1284["MDL"]))
		out = append(out, c)
	}
	return out, nil
}

func byUSB(db *devicedb.DB, usbID, mdl string) *devicedb.Model {
	var byMDL *devicedb.Model
	for _, id := range db.ModelIDs() {
		m, _ := db.Model(id)
		if slices.Contains(m.Match.USB, usbID) {
			return m
		}
		if mdl != "" && m.Match.IEEE1284Model == mdl {
			byMDL = m
		}
	}
	return byMDL
}

// Options for Network. Zero values: the host's subnets (each at most a /24), port 9100,
// 300 ms per connect, 64 at a time, 30 s in all, real TCP and SNMP (community public).
type Options struct {
	Subnets     []netip.Prefix // IPv4, at most /22 each (1024 addresses)
	Timeout     time.Duration  // the whole scan
	Port        int
	DialTimeout time.Duration
	Workers     int
	Dial        func(ctx context.Context, addr string) (net.Conn, error)
	SysDescr    func(ctx context.Context, host string) (string, error) // nil = SNMP device description + sysDescr
}

// Network finds hosts with the print port open and names them from their SNMP sysDescr.
// It only connects and closes; nothing is sent to the print port.
func Network(ctx context.Context, db *devicedb.DB, o Options) ([]Candidate, error) {
	if o.Subnets == nil {
		o.Subnets = LocalSubnets()
	}
	for _, p := range o.Subnets {
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("subnet %s: only IPv4 networks can be scanned", p)
		}
		if p.Bits() < 22 {
			return nil, fmt.Errorf("subnet %s is too large to scan (at most /22, 1024 addresses)", p)
		}
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if o.Port == 0 {
		o.Port = 9100
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 300 * time.Millisecond
	}
	if o.Workers == 0 {
		o.Workers = 64
	}
	if o.Dial == nil {
		var d net.Dialer
		o.Dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	if o.SysDescr == nil {
		o.SysDescr = func(ctx context.Context, host string) (string, error) {
			return snmp.Describe(ctx, host, "public", time.Second)
		}
	}
	var (
		mu   sync.Mutex
		out  []Candidate
		wg   sync.WaitGroup
		sema = make(chan struct{}, o.Workers)
	)
scan:
	for _, p := range o.Subnets {
		for a := p.Masked().Addr(); p.Contains(a); a = a.Next() {
			select {
			case sema <- struct{}{}:
			case <-ctx.Done():
				break scan
			}
			wg.Add(1)
			go func(a netip.Addr) {
				defer func() { <-sema; wg.Done() }()
				dctx, cancel := context.WithTimeout(ctx, o.DialTimeout)
				conn, err := o.Dial(dctx, net.JoinHostPort(a.String(), strconv.Itoa(o.Port)))
				cancel()
				if err != nil {
					return
				}
				_ = conn.Close()
				c := Candidate{Kind: "tcp", Host: a.String(), Port: o.Port}
				if descr, err := o.SysDescr(ctx, a.String()); err == nil {
					c.SNMPDescr = descr
				}
				c.setModel(bySNMP(db, c.SNMPDescr))
				mu.Lock()
				out = append(out, c)
				mu.Unlock()
			}(a)
		}
	}
	wg.Wait()
	slices.SortFunc(out, func(x, y Candidate) int {
		return netip.MustParseAddr(x.Host).Compare(netip.MustParseAddr(y.Host))
	})
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, nil // the time limit just ends the scan
	}
	return out, ctx.Err()
}

func bySNMP(db *devicedb.DB, descr string) *devicedb.Model {
	if descr == "" {
		return nil
	}
	for _, id := range db.ModelIDs() {
		m, _ := db.Model(id)
		if m.MatchesSNMP(descr) {
			return m
		}
	}
	return nil
}

// LocalSubnets are the host's non-loopback IPv4 subnets, each at most a /24.
func LocalSubnets() []netip.Prefix {
	var ps []netip.Prefix
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if p, err := netip.ParsePrefix(n.String()); err == nil && p.Addr().Is4() {
					ps = append(ps, p)
				}
			}
		}
	}
	return capTo24(ps)
}

// capTo24 narrows each prefix to at most the /24 around its address (no host-wide /16 scans).
func capTo24(ps []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range ps {
		bits := max(p.Bits(), 24)
		q, _ := p.Addr().Prefix(bits)
		if !slices.Contains(out, q) {
			out = append(out, q)
		}
	}
	return out
}

// TransportLine is the config line that selects this printer.
func TransportLine(c Candidate) string {
	if c.Kind == "usb" {
		if c.Serial != "" {
			return fmt.Sprintf("transport: {kind: usb, usb_id: %q, serial: %q}", c.USBID, c.Serial)
		}
		return fmt.Sprintf("transport: {kind: usb, usb_id: %q}", c.USBID)
	}
	if c.Port != 0 && c.Port != 9100 {
		return fmt.Sprintf("transport: {kind: tcp, host: %s, port: %d}", c.Host, c.Port)
	}
	return fmt.Sprintf("transport: {kind: tcp, host: %s}", c.Host)
}
