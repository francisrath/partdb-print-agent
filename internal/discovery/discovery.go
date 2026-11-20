// Package discovery finds printers on USB (Linux sysfs), the local network and Bluetooth LE, and names
// them from the device database. It is passive: it never writes to a printer.
package discovery

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
	"github.com/francisrath/partdb-print-agent/internal/transport/ble"
	"github.com/francisrath/partdb-print-agent/internal/transport/usb"
)

// Candidate statuses.
const (
	Supported = "supported" // known model with a driver in this build
	NoDriver  = "no-driver" // known model, no driver yet
	Unknown   = "unknown"   // not in the device database (or didn't say what it is)
)

type Candidate struct {
	Kind      string            `json:"kind"` // usb | tcp | ble
	Device    string            `json:"device,omitempty"`
	Host      string            `json:"host,omitempty"`
	Port      int               `json:"port,omitempty"`
	USBID     string            `json:"usb_id,omitempty"`
	Serial    string            `json:"serial,omitempty"`
	IEEE1284  map[string]string `json:"ieee1284,omitempty"`
	SNMPDescr string            `json:"snmp_descr,omitempty"`
	Address   string            `json:"address,omitempty"`  // ble: MAC, or a CoreBluetooth UUID on macOS
	BLEName   string            `json:"ble_name,omitempty"` // ble: the advertised name
	Model     *devicedb.Model   `json:"-"`
	ModelID   string            `json:"model,omitempty"`
	Status    string            `json:"status"`
}

// Name is how to show the printer: its model, else what it says about itself (IEEE 1284, or the
// SNMP description up to its first ";": Brother appends the network card and firmware, or the
// advertised Bluetooth name), else "?".
func (c *Candidate) Name() string {
	name := cmp.Or(c.ModelID, c.BLEName)
	if name == "" && c.IEEE1284["MDL"] != "" {
		name = strings.TrimSpace(c.IEEE1284["MFG"] + " " + c.IEEE1284["MDL"])
	}
	if name == "" {
		name, _, _ = strings.Cut(c.SNMPDescr, ";")
		name = strings.TrimSpace(name)
	}
	if name == "" {
		return "?"
	}
	return name
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
	if byMDL == nil {
		byMDL, _ = db.ModelByZebraMDL(mdl) // Zebras without a known USB id
	}
	return byMDL
}

// Options for Network and BLE. Network's zero values: the host's subnets (each at most a /24), port
// 9100, 300 ms per connect, 64 at a time, 30 s in all, real TCP and SNMP (community public). BLE uses
// only Timeout (0 = 8 s) and ScanBLE.
type Options struct {
	Subnets     []netip.Prefix // IPv4, at most /22 each (1024 addresses)
	Timeout     time.Duration  // the whole scan (BLE: how long to listen)
	Port        int
	DialTimeout time.Duration
	Workers     int
	Dial        func(ctx context.Context, addr string) (net.Conn, error)
	SysDescr    func(ctx context.Context, host string) (string, error) // nil = SNMP device description + sysDescr
	// ScanBLE lists the Bluetooth LE devices seen within timeout (BLE); nil = ble.Scan.
	ScanBLE func(ctx context.Context, timeout time.Duration) ([]blecore.Advert, error)
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

// BLE lists Bluetooth LE printers from a passive scan of o.Timeout (0 = 8 s): devices whose advertised
// name matches a model, and unknown ones advertising the cat printers' service ae30. Without a usable
// adapter the error wraps blecore.ErrNoBluetooth (see BLENote).
func BLE(ctx context.Context, db *devicedb.DB, o Options) ([]Candidate, error) {
	if o.Timeout == 0 {
		o.Timeout = 8 * time.Second
	}
	if o.ScanBLE == nil {
		o.ScanBLE = ble.Scan
	}
	ads, err := o.ScanBLE(ctx, o.Timeout)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, a := range ads {
		m, _ := db.ModelByBLEName(a.Name)
		if m == nil && !slices.ContainsFunc(a.Services, isCatService) {
			continue // phones, headphones, …
		}
		c := Candidate{Kind: "ble", Address: a.Address, BLEName: a.Name}
		c.setModel(m)
		out = append(out, c)
	}
	return out, nil
}

// isCatService: service ae30, short or in the 128-bit Bluetooth base form.
func isCatService(u string) bool {
	u = strings.ToLower(u)
	return u == "ae30" || u == "0000ae30-0000-1000-8000-00805f9b34fb"
}

// BLENote says why Bluetooth printers weren't searched, for a line beside the other results.
func BLENote(err error) string {
	if errors.Is(err, blecore.ErrNoBluetooth) {
		return "Bluetooth printers not searched: Bluetooth is off or this computer has none (" + err.Error() + ")"
	}
	return "Bluetooth printers not searched: " + err.Error()
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
	if c.Kind == "ble" {
		return fmt.Sprintf("transport: {kind: ble, name: %q, address: %q}", c.BLEName, c.Address)
	}
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

// Identify asks a network printer what it is with Zebra's ~HI. Call it only after the user said
// yes: a printer that isn't a Zebra may print the few bytes as a page. The reply names the model
// and its dots per mm; reply is the raw answer ("" when there was none).
func Identify(ctx context.Context, db *devicedb.DB, c Candidate) (Candidate, string, error) {
	port := c.Port
	if port == 0 {
		port = 9100
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(port)))
	if err != nil {
		return c, "", err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("~HI\r\n")); err != nil {
		return c, "", err
	}
	var reply []byte
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for !bytes.Contains(reply, []byte{0x03}) && !bytes.Contains(reply, []byte{'\n'}) && len(reply) < 4096 {
		n, err := conn.Read(buf)
		reply = append(reply, buf[:n]...)
		if err != nil {
			break
		}
	}
	if len(bytes.TrimSpace(reply)) == 0 {
		return c, "", errors.New(`no answer to ~HI: not a Zebra, or a Zebra set to EPL only (on the printer: ! U1 setvar "device.languages" "zpl")`)
	}
	m, ok := db.ModelByHI(string(reply))
	if !ok {
		return c, string(reply), fmt.Errorf("a printer we don't know yet answered: %q", strings.TrimSpace(strings.Trim(string(reply), "\x02\x03")))
	}
	c.setModel(m)
	return c, string(reply), nil
}
