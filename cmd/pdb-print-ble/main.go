//go:build darwin && cgo

// Command pdb-print-ble is pdb-print's Bluetooth helper on macOS. The Bluetooth library asks macOS for
// Bluetooth permission as soon as it is loaded, so pdb-print itself never links it; it starts this helper
// for each BLE job instead (see internal/blebridge for the protocol on stdin/stdout).
//
//	pdb-print-ble                     bridge mode: frames on stdin/stdout, logs on stderr
//	pdb-print-ble scan --timeout 8s   one JSON advert per line
//
// Build with the embedded Info.plist (NSBluetoothAlwaysUsageDescription):
//
//	CGO_ENABLED=1 go build -ldflags "-linkmode=external -extldflags '-sectcreate __TEXT __info_plist cmd/pdb-print-ble/Info.plist'" ./cmd/pdb-print-ble
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/francisrath/partdb-print-agent/internal/blebridge"
	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/bleradio"
)

// fatalMark starts the last stderr line when the helper exits on an error (internal/transport/ble reads it).
const fatalMark = "error: "

const (
	openScan       = 10 * time.Second
	connectTimeout = 10 * time.Second
)

var adapter = bluetooth.DefaultAdapter

func main() {
	log.SetFlags(0)
	log.SetPrefix("pdb-print-ble: ")
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "scan":
		err = scanCmd(os.Args[2:])
	case len(os.Args) > 1:
		fmt.Fprintln(os.Stderr, "usage: pdb-print-ble [scan --timeout 8s]")
		os.Exit(2)
	default:
		err = serve(os.Stdin, &frames{w: os.Stdout})
	}
	if err != nil {
		// The marked last stderr line is the cause pdb-print reports (and how it recognises ErrNoBluetooth).
		fmt.Fprintln(os.Stderr, fatalMark+err.Error())
		os.Exit(1)
	}
}

func scanCmd(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 8*time.Second, "how long to listen")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ads, err := scan(ctx, *timeout, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, ad := range ads {
		if err := enc.Encode(ad); err != nil {
			return err
		}
	}
	return nil
}

// enable waits up to a minute: on the first run macOS shows a permission prompt and Bluetooth stays
// unavailable until it is answered.
func enable() error { return blecore.WaitEnable(adapter.Enable, blecore.EnableWait) }

// scan listens until timeout, ctx ends, or settled reports true.
func scan(ctx context.Context, timeout time.Duration, settled func(*blecore.Scanned, time.Time) bool) ([]blecore.Advert, error) {
	enabled := make(chan error, 1)
	go func() { enabled <- enable() }() // may wait for the permission prompt
	select {
	case err := <-enabled:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done(): // interrupted before the scan started, perhaps with a permission prompt still open
		return nil, blecore.ErrNotEnabled
	}
	ads, err := bleradio.Scan(ctx, adapter, timeout, settled)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return ads, nil
}

// frames writes frames to stdout from the request loop and the notification callback.
type frames struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frames) send(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return blebridge.Write(f.w, typ, p)
}

// serve answers one connection: Open, then Writes until Close or end of input.
func serve(in io.Reader, out *frames) error {
	typ, p, err := blebridge.Read(in)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if typ != blebridge.TypeOpen {
		return fmt.Errorf("expected an open frame, got %q", typ)
	}
	var req blebridge.OpenRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return fmt.Errorf("open request: %w", err)
	}
	c, err := open(req, out)
	if err != nil {
		_ = out.send(blebridge.TypeError, []byte(err.Error()))
		return err
	}
	defer c.close()
	ch := c.ch
	ok, _ := json.Marshal(blebridge.OK{MTU: c.mtu})
	if err := out.send(blebridge.TypeOK, ok); err != nil {
		return err
	}
	for {
		typ, p, err := blebridge.Read(in)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch typ {
		case blebridge.TypeClose:
			return nil
		case blebridge.TypeWrite:
			answer, msg := blebridge.TypeOK, []byte(nil)
			if err := ch.Send(p); err != nil {
				log.Printf("write: %v", err)
				answer, msg = blebridge.TypeError, []byte(err.Error())
			}
			if err := out.send(answer, msg); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected frame %q", typ)
		}
	}
}

// conn is the open connection serve uses.
type conn struct {
	dev  bluetooth.Device
	rx   *bluetooth.DeviceCharacteristic
	ch   *blecore.Chunker
	mtu  int
	link *blecore.Link
}

// close switches notifications off, disconnects and waits for CoreBluetooth to confirm: Disconnect only
// asks for it, and a helper that exits at once can leave the printer connected (it then doesn't advertise,
// so the next job can't find it).
func (c *conn) close() {
	c.link.Closing()
	off := make(chan struct{})
	go func() { _ = c.rx.EnableNotifications(nil); close(off) }()
	select {
	case <-off:
	case <-time.After(blecore.NotifyOffWait):
	}
	_ = c.dev.Disconnect()
	if !c.link.Wait(blecore.DisconnectWait) {
		log.Printf("disconnect not confirmed within %v", blecore.DisconnectWait)
	}
}

// open scans for the printer (settling on a name like the in-process transport), connects and subscribes.
// Every notification goes to the client; the chunker also sees the flow control.
func open(req blebridge.OpenRequest, out *frames) (*conn, error) {
	if req.Name == "" && req.Address == "" {
		return nil, errors.New("set the printer's address or name")
	}
	found, err := scan(context.Background(), openScan, func(s *blecore.Scanned, now time.Time) bool {
		return s.Settled(req.Name, req.Address, now)
	})
	if err != nil {
		return nil, err
	}
	ad, err := blecore.Pick(found, req.Name, req.Address)
	var missed error // the scan missed the configured address
	if errors.Is(err, blecore.ErrNotFound) && req.Address != "" {
		// A printer still connected (macOS may hold the link a while after the last job) doesn't
		// advertise, but Connect finds a peripheral the system knows by its UUID.
		log.Printf("%s not seen in the scan, connecting by its address", req.Address)
		missed, ad, err = blecore.NotFound(err, openScan), blecore.Advert{Address: req.Address}, nil
	}
	if errors.Is(err, blecore.ErrNotFound) {
		return nil, blecore.NotFound(err, openScan)
	}
	if err != nil {
		return nil, err
	}
	id, err := bluetooth.ParseUUID(ad.Address)
	if err != nil {
		return nil, fmt.Errorf("address %q: %w", ad.Address, err)
	}
	// When the link drops, the client learns from this process ending (and its last stderr line). The
	// handler is in place before Connect so a drop during setup isn't missed; disconnects before Connect
	// succeeded belong to a failed attempt (Connect reports those itself), and those after close started
	// are our own.
	var (
		connected atomic.Bool
		live      atomic.Pointer[blecore.Chunker]
		link      = blecore.NewLink()
	)
	adapter.SetConnectionStateHandler(func(d bluetooth.Device, s bluetooth.ConnectionState) {
		if s.Connected || d.Address.UUID != id || !connected.Load() || !link.Dropped() {
			return
		}
		if ch := live.Load(); ch != nil {
			ch.Close()
		}
		fmt.Fprintf(os.Stderr, "%sprinter %s disconnected\n", fatalMark, ad.Address)
		os.Exit(1)
	})
	dev, err := adapter.Connect(bluetooth.Address{UUID: id},
		bluetooth.ConnectionParams{ConnectionTimeout: bluetooth.NewDuration(connectTimeout)})
	if err != nil {
		if missed != nil {
			return nil, fmt.Errorf("%w; connecting by address: %v", missed, err)
		}
		return nil, fmt.Errorf("connect %s: %w", ad.Address, err)
	}
	connected.Store(true)
	if ok, _ := dev.Connected(); !ok { // dropped before the handler could count it
		return nil, fmt.Errorf("printer %s disconnected", ad.Address)
	}
	ch, rx, mtu, err := setup(dev, req, out)
	if err != nil {
		link.Closing()
		_ = dev.Disconnect()
		link.Wait(blecore.DisconnectWait)
		return nil, err
	}
	live.Store(ch)
	who := ad.Address
	if ad.Name != "" { // found by address, the advert may carry no name
		who += " (" + ad.Name + ")"
	}
	log.Printf("connected to %s, %d bytes per write", who, mtu)
	return &conn{dev: dev, rx: rx, ch: ch, mtu: mtu, link: link}, nil
}

func setup(dev bluetooth.Device, req blebridge.OpenRequest, out *frames) (*blecore.Chunker, *bluetooth.DeviceCharacteristic, int, error) {
	service := bluetooth.New16BitUUID(req.Service)
	write, notify := bluetooth.New16BitUUID(req.Write), bluetooth.New16BitUUID(req.Notify)
	tx, rx, err := bleradio.Characteristics(dev, service, write, notify)
	if err != nil {
		return nil, nil, 0, err
	}
	// macOS reports the writable length itself (no −3), and WriteWithoutResponse waits for buffer space.
	mtu, err := tx.GetMTU()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("mtu: %w", err)
	}
	ch := &blecore.Chunker{Max: int(mtu), Gap: time.Duration(req.GapMS) * time.Millisecond, Write: func(b []byte) error {
		_, err := tx.WriteWithoutResponse(b)
		return err
	}}
	if err := rx.EnableNotifications(func(b []byte) {
		ch.OnNotify(b)
		if err := out.send(blebridge.TypeNotify, b); err != nil {
			log.Printf("notification: %v", err)
		}
	}); err != nil {
		return nil, nil, 0, fmt.Errorf("enable notifications: %w", err)
	}
	return ch, rx, max(int(mtu), 20), nil
}
