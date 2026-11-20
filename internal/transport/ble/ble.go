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
	minMTU         = 23              // the ATT default; Windows reports it until the MTU exchange is done
	retryPause     = 2 * time.Second // before the one connect retry (500 ms failed right after a paper jam)
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

	liveMu sync.Mutex
	live   = map[string]*blecore.Chunker{} // open connections by address, for the disconnect handler
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
	// A dropped link fails a paused Send at once. Windows reports remote drops here; v0.17.0's Linux
	// central only reports our own Disconnect (a write to a dropped link fails there anyway).
	adapter.SetConnectionStateHandler(func(d bluetooth.Device, s bluetooth.ConnectionState) {
		if s.Connected {
			return
		}
		liveMu.Lock()
		c := live[d.Address.String()]
		liveMu.Unlock()
		if c != nil {
			c.Close()
		}
	})
	enabled = true
	return nil
}

// Scan listens for adverts for up to timeout and returns every device seen once (by address). Without a
// usable adapter (none, BlueZ down, powered off) the error wraps blecore.ErrNoBluetooth.
func Scan(ctx context.Context, timeout time.Duration) ([]blecore.Advert, error) {
	return scan(ctx, timeout, nil)
}

// scan stops early once settled reports true.
func scan(ctx context.Context, timeout time.Duration, settled func(*blecore.Scanned, time.Time) bool) ([]blecore.Advert, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	scanMu.Lock()
	defer scanMu.Unlock()
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
		case err := <-errc: // the scan ended by itself
			if err == nil {
				return results(), nil
			}
			return nil, scanError(err)
		case <-ctx.Done():
			stopScan(errc) // an error while stopping doesn't spoil what was found
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

// stopScan stops the scan and waits for the library's Scan to return. StopScan fails while Scan is still
// starting and may be lost on Windows before the watcher has started, so it is repeated until Scan returns.
// Once stopped, v0.17.0 returns promptly: BlueZ's loop sees the closed cancel channel at once, Windows
// waits for the watcher's Stopped event.
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

// Transport is one BLE connection to a cat printer.
type Transport struct {
	Address, Name          string // the address wins (see config.Transport)
	service, write, notify bluetooth.UUID
	gap                    time.Duration

	mu      sync.Mutex // guards everything below; onNotify runs on the library's goroutine
	dev     bluetooth.Device
	addr    string
	tx, rx  bluetooth.DeviceCharacteristic
	chunker *blecore.Chunker
	mtu     int  // last ATT MTU read; re-read while it is still the default
	open    bool // the chunker outlives Close: a late notification may still reach onNotify
	buf     []byte
	ready   chan struct{} // signalled when buf gains bytes
}

func (t *Transport) Kind() string { return "ble" }

func (t *Transport) Open(ctx context.Context) error {
	connectMu.Lock()
	defer connectMu.Unlock()
	if _, err := blecore.Pick(nil, t.Name, t.Address); !errors.Is(err, blecore.ErrNotFound) {
		return err // neither name nor address set
	}
	found, err := scan(ctx, openScan, func(s *blecore.Scanned, now time.Time) bool {
		return s.Settled(t.Name, t.Address, now)
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
			return blecore.NotFound(err, openScan)
		}
		return err
	}
	mac, err := bluetooth.ParseMAC(ad.Address)
	if err != nil {
		return fmt.Errorf("ble: address %q: %w", ad.Address, err)
	}
	addr := bluetooth.Address{MACAddress: bluetooth.MACAddress{MAC: mac}}

	// BlueZ sometimes aborts the first attempt at once (le-connection-abort-by-local): drop any stale link
	// BlueZ still holds, wait and retry once. Not after a timeout – the first Connect may still be running on
	// the same device, and its late Disconnect would drop the retry's link.
	var dev bluetooth.Device
	err = blecore.ConnectRetry(ctx, retryPause, func() (err error) {
		dev, err = connect(ctx, addr)
		return err
	}, func(err error) bool { return !errors.Is(err, errConnectTimeout) }, func() { dropStale(ctx, addr) })
	if err != nil {
		return fmt.Errorf("ble: connect %s: %w", ad.Address, err)
	}
	if err := t.setup(dev, addr.String()); err != nil {
		_ = dev.Disconnect()
		return err
	}
	return nil
}

var errConnectTimeout = fmt.Errorf("no connection within %v", connectTimeout)

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
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case r := <-res:
		return r.dev, r.err
	case <-timer.C:
	case <-ctx.Done():
	}
	go func() {
		if r := <-res; r.err == nil {
			_ = r.dev.Disconnect()
		}
	}()
	if err := ctx.Err(); err != nil {
		return bluetooth.Device{}, err
	}
	return bluetooth.Device{}, errConnectTimeout
}

func (t *Transport) setup(dev bluetooth.Device, addr string) error {
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{t.service})
	if err != nil || len(svcs) == 0 {
		return fmt.Errorf("ble: printer has no service %s: %v", t.service, err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{t.write, t.notify})
	if err != nil {
		return fmt.Errorf("ble: characteristics: %w", err)
	}
	var tx, rx *bluetooth.DeviceCharacteristic
	for i := range chars {
		switch chars[i].UUID() {
		case t.write:
			tx = &chars[i]
		case t.notify:
			rx = &chars[i]
		}
	}
	if tx == nil || rx == nil {
		return fmt.Errorf("ble: printer lacks characteristic %s or %s", t.write, t.notify)
	}
	write := *tx
	ch := &blecore.Chunker{Max: 20, Gap: t.gap, Write: func(b []byte) error {
		_, err := write.WriteWithoutResponse(b)
		return err
	}}
	t.mu.Lock()
	t.dev, t.addr, t.tx, t.rx = dev, addr, *tx, *rx
	t.chunker, t.mtu, t.buf, t.ready = ch, 0, nil, make(chan struct{}, 1)
	t.mu.Unlock()
	// t.rx keeps the subscription's state (Linux), so enable it on the field, not a copy. Nothing else
	// touches t.rx until open is set.
	if err := t.rx.EnableNotifications(t.onNotify); err != nil {
		return fmt.Errorf("ble: enable notifications: %w", err)
	}
	liveMu.Lock()
	live[addr] = ch
	liveMu.Unlock()
	t.mu.Lock()
	t.open = true
	t.mu.Unlock()
	return nil
}

// onNotify drives the flow control and buffers every notification (flow control included) for Read.
func (t *Transport) onNotify(b []byte) {
	t.mu.Lock()
	ch, ready := t.chunker, t.ready
	t.buf = append(t.buf, b...)
	t.mu.Unlock()
	ch.OnNotify(b)
	select {
	case ready <- struct{}{}:
	default:
	}
}

// Write returns once the last chunk has gone out.
func (t *Transport) Write(p []byte) (int, error) {
	t.mu.Lock()
	if !t.open {
		t.mu.Unlock()
		return 0, errors.New("ble: not open")
	}
	ch := t.chunker
	reread, tx := t.mtu <= minMTU, t.tx // Windows says 23 until the MTU exchange is done
	t.mu.Unlock()
	if reread {
		if mtu, err := tx.GetMTU(); err == nil {
			t.mu.Lock()
			t.mtu = int(mtu)
			ch.Max = t.mtu - 3 // chunk = ATT MTU − 3; the floor of 20 is the chunker's
			t.mu.Unlock()
		}
	}
	if err := ch.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read returns buffered notification bytes; with nothing for 2 s, an error wrapping os.ErrDeadlineExceeded.
func (t *Transport) Read(p []byte) (int, error) {
	timer := time.NewTimer(readTimeout)
	defer timer.Stop()
	for {
		t.mu.Lock()
		if !t.open {
			t.mu.Unlock()
			return 0, errors.New("ble: not open")
		}
		if len(t.buf) > 0 {
			n := copy(p, t.buf)
			t.buf = t.buf[n:]
			t.mu.Unlock()
			return n, nil
		}
		ready := t.ready
		t.mu.Unlock()
		select {
		case <-ready:
		case <-timer.C:
			return 0, fmt.Errorf("ble: no notification within %v: %w", readTimeout, os.ErrDeadlineExceeded)
		}
	}
}

func (t *Transport) Close() error {
	t.mu.Lock()
	if !t.open {
		t.mu.Unlock()
		return nil
	}
	t.open = false
	ch, dev, addr := t.chunker, t.dev, t.addr
	t.mu.Unlock()
	ch.Close()
	liveMu.Lock()
	if live[addr] == ch {
		delete(live, addr)
	}
	liveMu.Unlock()
	_ = t.rx.EnableNotifications(nil) // open is false, so nothing else touches t.rx
	return dev.Disconnect()
}
