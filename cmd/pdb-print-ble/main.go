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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		mu   sync.Mutex
		seen blecore.Scanned
	)
	errc := make(chan error, 1)
	go func() {
		errc <- adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
			ad := blecore.Advert{Name: r.LocalName(), Address: r.Address.String(), RSSI: int(r.RSSI)}
			for _, u := range r.ServiceUUIDs() {
				ad.Services = append(ad.Services, u.String())
			}
			mu.Lock()
			seen.Add(ad, time.Now())
			mu.Unlock()
		})
	}()
	results := func() []blecore.Advert {
		mu.Lock()
		defer mu.Unlock()
		return seen.Adverts()
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			if err != nil {
				return nil, fmt.Errorf("scan: %w", err)
			}
			return results(), nil
		case <-ctx.Done():
			stopScan(errc)
			return results(), nil
		case <-tick.C:
			mu.Lock()
			done := settled != nil && settled(&seen, time.Now())
			mu.Unlock()
			if done {
				stopScan(errc)
				return results(), nil
			}
		}
	}
}

// stopScan repeats StopScan until Scan returns: it fails while Scan is still starting.
func stopScan(errc <-chan error) {
	for {
		_ = adapter.StopScan()
		select {
		case <-errc:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
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
	dev, ch, mtu, err := open(req, out)
	if err != nil {
		_ = out.send(blebridge.TypeError, []byte(err.Error()))
		return err
	}
	defer func() { _ = dev.Disconnect() }()
	ok, _ := json.Marshal(blebridge.OK{MTU: mtu})
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

// open scans for the printer (settling on a name like the in-process transport), connects and subscribes.
// Every notification goes to the client; the chunker also sees the flow control.
func open(req blebridge.OpenRequest, out *frames) (bluetooth.Device, *blecore.Chunker, int, error) {
	var none bluetooth.Device
	if req.Name == "" && req.Address == "" {
		return none, nil, 0, errors.New("set the printer's address or name")
	}
	found, err := scan(context.Background(), openScan, func(s *blecore.Scanned, now time.Time) bool {
		return s.Settled(req.Name, req.Address, now)
	})
	if err != nil {
		return none, nil, 0, err
	}
	ad, err := blecore.Pick(found, req.Name, req.Address)
	if errors.Is(err, blecore.ErrNotFound) {
		return none, nil, 0, fmt.Errorf("%w within %v (is it on, and not connected to a phone?)", err, openScan)
	}
	if err != nil {
		return none, nil, 0, err
	}
	id, err := bluetooth.ParseUUID(ad.Address)
	if err != nil {
		return none, nil, 0, fmt.Errorf("address %q: %w", ad.Address, err)
	}
	// When the link drops, the client learns from this process ending (and its last stderr line). The
	// handler is in place before Connect so a drop during setup isn't missed; disconnects before Connect
	// succeeded belong to a failed attempt (Connect reports those itself).
	var (
		connected atomic.Bool
		live      atomic.Pointer[blecore.Chunker]
	)
	adapter.SetConnectionStateHandler(func(d bluetooth.Device, s bluetooth.ConnectionState) {
		if s.Connected || d.Address.UUID != id || !connected.Load() {
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
		return none, nil, 0, fmt.Errorf("connect %s: %w", ad.Address, err)
	}
	connected.Store(true)
	if ok, _ := dev.Connected(); !ok { // dropped before the handler could count it
		return none, nil, 0, fmt.Errorf("printer %s disconnected", ad.Address)
	}
	ch, mtu, err := setup(dev, req, out)
	if err != nil {
		_ = dev.Disconnect()
		return none, nil, 0, err
	}
	live.Store(ch)
	log.Printf("connected to %s (%s), %d bytes per write", ad.Address, ad.Name, mtu)
	return dev, ch, mtu, nil
}

func setup(dev bluetooth.Device, req blebridge.OpenRequest, out *frames) (*blecore.Chunker, int, error) {
	service := bluetooth.New16BitUUID(req.Service)
	write, notify := bluetooth.New16BitUUID(req.Write), bluetooth.New16BitUUID(req.Notify)
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{service})
	if err != nil || len(svcs) == 0 {
		return nil, 0, fmt.Errorf("printer has no service %s: %v", service, err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{write, notify})
	if err != nil {
		return nil, 0, fmt.Errorf("characteristics: %w", err)
	}
	var tx, rx *bluetooth.DeviceCharacteristic
	for i := range chars {
		switch chars[i].UUID() {
		case write:
			tx = &chars[i]
		case notify:
			rx = &chars[i]
		}
	}
	if tx == nil || rx == nil {
		return nil, 0, fmt.Errorf("printer lacks characteristic %s or %s", write, notify)
	}
	// macOS reports the writable length itself (no −3), and WriteWithoutResponse waits for buffer space.
	mtu, err := tx.GetMTU()
	if err != nil {
		return nil, 0, fmt.Errorf("mtu: %w", err)
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
		return nil, 0, fmt.Errorf("enable notifications: %w", err)
	}
	return ch, max(int(mtu), 20), nil
}
