//go:build linux || windows

package ble

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport"
)

const (
	openScan       = 10 * time.Second // BlueZ forgets unpaired LE devices, so every Open scans first
	connectTimeout = 10 * time.Second // Linux Connect ignores ConnectionParams.ConnectionTimeout
	readTimeout    = 2 * time.Second
)

func init() {
	transport.Register("ble", func(c config.Transport, m *devicedb.Model) (driver.Transport, error) {
		q := func(key string, def int) int {
			if m == nil {
				return def
			}
			return m.Quirk(key, def)
		}
		return &Transport{
			Address: c.Address,
			Name:    c.Name,
			service: bluetooth.New16BitUUID(uint16(q("ble_service", 0xae30))),
			write:   bluetooth.New16BitUUID(uint16(q("ble_write", 0xae01))),
			notify:  bluetooth.New16BitUUID(uint16(q("ble_notify", 0xae02))),
			gap:     time.Duration(q("ble_gap_ms", 10)) * time.Millisecond,
		}, nil
	})
}

// The library has one process-wide adapter. Windows' Enable (RoInitialize) must succeed only once per
// process, and only one scan may run at a time.
var (
	adapter   = bluetooth.DefaultAdapter
	enableMu  sync.Mutex
	enabled   bool
	scanMu    sync.Mutex
	connectMu sync.Mutex // Open's scan + connect, so two jobs don't race for the adapter
)

func enable() error {
	enableMu.Lock()
	defer enableMu.Unlock()
	if enabled {
		return nil
	}
	if err := adapter.Enable(); err != nil {
		return fmt.Errorf("%w: %w", blecore.ErrNoBluetooth, err)
	}
	enabled = true
	return nil
}

// Scan listens for adverts for up to timeout and returns every device seen once (by address, latest
// advert wins). Without a usable adapter the error wraps blecore.ErrNoBluetooth.
func Scan(ctx context.Context, timeout time.Duration) ([]blecore.Advert, error) {
	return scan(ctx, timeout, nil)
}

// scan stops early once done reports true for the adverts so far.
func scan(ctx context.Context, timeout time.Duration, done func([]blecore.Advert) bool) ([]blecore.Advert, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	scanMu.Lock()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		mu      sync.Mutex
		found   []blecore.Advert
		index   = map[string]int{}
		stopped bool
	)
	errc := make(chan error, 1)
	go func() {
		errc <- adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
			mu.Lock()
			defer mu.Unlock()
			if stopped {
				return
			}
			ad := blecore.Advert{Name: r.LocalName(), Address: r.Address.String(), RSSI: int(r.RSSI)}
			for _, u := range r.ServiceUUIDs() {
				ad.Services = append(ad.Services, u.String())
			}
			if i, ok := index[ad.Address]; ok {
				if ad.Name == "" { // a scan response or RSSI update without the name
					ad.Name = found[i].Name
				}
				if ad.Services == nil {
					ad.Services = found[i].Services
				}
				found[i] = ad
			} else {
				index[ad.Address] = len(found)
				found = append(found, ad)
			}
			if done != nil && done(found) {
				stopped = true
				_ = a.StopScan()
			}
		})
	}()

	var err error
	select {
	case err = <-errc:
		scanMu.Unlock()
	case <-ctx.Done():
		err = stopScan(&mu, &stopped, errc)
	}
	mu.Lock()
	defer mu.Unlock()
	stopped = true
	if err != nil {
		return nil, fmt.Errorf("%w: scan: %w", blecore.ErrNoBluetooth, err)
	}
	return append([]blecore.Advert(nil), found...), nil
}

// stopScan stops a running scan and unlocks scanMu once the library's Scan has returned. StopScan fails
// while Scan is still starting, so it retries. BlueZ's Scan only notices the stop on its next D-Bus signal,
// so after 2 s the results are returned anyway and the next scan waits for the old one in scanMu.
func stopScan(mu *sync.Mutex, stopped *bool, errc <-chan error) error {
	for {
		mu.Lock()
		*stopped = true
		err := adapter.StopScan()
		mu.Unlock()
		if err == nil {
			break
		}
		select {
		case err := <-errc:
			scanMu.Unlock()
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case err := <-errc:
		scanMu.Unlock()
		return err
	case <-time.After(2 * time.Second):
		go func() { <-errc; scanMu.Unlock() }()
		return nil
	}
}

// Transport is one BLE connection to a cat printer.
type Transport struct {
	Address, Name          string // the address wins (see config.Transport)
	service, write, notify bluetooth.UUID
	gap                    time.Duration

	dev     bluetooth.Device
	tx, rx  bluetooth.DeviceCharacteristic
	chunker *blecore.Chunker
	sized   bool // chunk size read from the MTU (Windows reports 23 right after connecting)
	open    bool // the chunker outlives Close: a late notification may still reach onNotify

	mu    sync.Mutex
	buf   []byte
	ready chan struct{} // signalled when buf gains bytes
}

func (t *Transport) Kind() string { return "ble" }

func (t *Transport) Open(ctx context.Context) error {
	connectMu.Lock()
	defer connectMu.Unlock()
	if _, err := blecore.Pick(nil, t.Name, t.Address); !errors.Is(err, blecore.ErrNotFound) {
		return err // neither name nor address set
	}
	found, err := scan(ctx, openScan, func(ads []blecore.Advert) bool {
		_, err := blecore.Pick(ads, t.Name, t.Address)
		return !errors.Is(err, blecore.ErrNotFound) // stop at the first match (or an ambiguous name)
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ad, err := blecore.Pick(found, t.Name, t.Address)
	if err != nil {
		if errors.Is(err, blecore.ErrNotFound) {
			return fmt.Errorf("%w within %v (is it on, and not connected to a phone?)", err, openScan)
		}
		return err
	}
	mac, err := bluetooth.ParseMAC(ad.Address)
	if err != nil {
		return fmt.Errorf("ble: address %q: %w", ad.Address, err)
	}
	addr := bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac}}

	dev, err := connect(ctx, addr)
	if err != nil { // BlueZ sometimes aborts the first attempt (le-connection-abort-by-local); retry once
		time.Sleep(500 * time.Millisecond)
		if dev, err = connect(ctx, addr); err != nil {
			return fmt.Errorf("ble: connect %s: %w", ad.Address, err)
		}
	}
	if err := t.setup(dev); err != nil {
		_ = dev.Disconnect()
		return err
	}
	return nil
}

// connect gives up after connectTimeout; a connection that still comes up later is dropped.
func connect(ctx context.Context, addr bluetooth.Address) (bluetooth.Device, error) {
	type result struct {
		dev bluetooth.Device
		err error
	}
	res := make(chan result, 1)
	go func() {
		d, err := adapter.Connect(addr, bluetooth.ConnectionParams{ConnectionTimeout: bluetooth.NewDuration(connectTimeout)})
		res <- result{d, err}
	}()
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	select {
	case r := <-res:
		return r.dev, r.err
	case <-ctx.Done():
		go func() {
			if r := <-res; r.err == nil {
				_ = r.dev.Disconnect()
			}
		}()
		return bluetooth.Device{}, fmt.Errorf("no connection within %v: %w", connectTimeout, ctx.Err())
	}
}

func (t *Transport) setup(dev bluetooth.Device) error {
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{t.service})
	if err != nil || len(svcs) == 0 {
		return fmt.Errorf("ble: printer has no service %s: %v", t.service, err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{t.write, t.notify})
	if err != nil {
		return fmt.Errorf("ble: characteristics: %w", err)
	}
	var haveTx, haveRx bool
	for _, c := range chars {
		switch c.UUID() {
		case t.write:
			t.tx, haveTx = c, true
		case t.notify:
			t.rx, haveRx = c, true
		}
	}
	if !haveTx || !haveRx {
		return fmt.Errorf("ble: printer lacks characteristic %s or %s", t.write, t.notify)
	}
	t.dev, t.open = dev, true
	t.buf, t.ready, t.sized = nil, make(chan struct{}, 1), false
	t.chunker = &blecore.Chunker{Max: 20, Gap: t.gap, Write: func(b []byte) error {
		_, err := t.tx.WriteWithoutResponse(b)
		return err
	}}
	if err := t.rx.EnableNotifications(t.onNotify); err != nil {
		return fmt.Errorf("ble: enable notifications: %w", err)
	}
	return nil
}

// onNotify drives the flow control and buffers every notification (flow control included) for Read.
func (t *Transport) onNotify(b []byte) {
	t.chunker.OnNotify(b)
	t.mu.Lock()
	t.buf = append(t.buf, b...)
	t.mu.Unlock()
	select {
	case t.ready <- struct{}{}:
	default:
	}
}

// Write returns once the last chunk has gone out.
func (t *Transport) Write(p []byte) (int, error) {
	if !t.open {
		return 0, errors.New("ble: not open")
	}
	if !t.sized { // chunk = ATT MTU − 3; the floor of 20 is the chunker's
		if mtu, err := t.tx.GetMTU(); err == nil {
			t.chunker.Max = int(mtu) - 3
		}
		t.sized = true
	}
	if err := t.chunker.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read returns buffered notification bytes; with nothing for 2 s, an error wrapping os.ErrDeadlineExceeded.
func (t *Transport) Read(p []byte) (int, error) {
	if !t.open {
		return 0, errors.New("ble: not open")
	}
	timer := time.NewTimer(readTimeout)
	defer timer.Stop()
	for {
		t.mu.Lock()
		if len(t.buf) > 0 {
			n := copy(p, t.buf)
			t.buf = t.buf[n:]
			t.mu.Unlock()
			return n, nil
		}
		t.mu.Unlock()
		select {
		case <-t.ready:
		case <-timer.C:
			return 0, fmt.Errorf("ble: no notification within %v: %w", readTimeout, os.ErrDeadlineExceeded)
		}
	}
}

func (t *Transport) Close() error {
	if !t.open {
		return nil
	}
	t.open = false
	_ = t.rx.EnableNotifications(nil)
	return t.dev.Disconnect()
}
