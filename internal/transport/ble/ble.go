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
	"github.com/francisrath/partdb-print-agent/internal/bleradio"
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
		var uuids [3]bluetooth.UUID
		for i, q := range []struct {
			key string
			def uint16
		}{{"ble_service", 0xae30}, {"ble_write", 0xae01}, {"ble_notify", 0xae02}} {
			u, err := bluetooth.ParseUUID(m.QuirkUUID(q.key, q.def))
			if err != nil {
				return nil, fmt.Errorf("ble: %s: %w", q.key, err)
			}
			uuids[i] = u
		}
		gap := 10
		if m != nil {
			gap = m.Quirk("ble_gap_ms", gap)
		}
		return &Transport{
			Address: c.Address,
			Name:    c.Name,
			service: uuids[0],
			write:   uuids[1],
			notify:  uuids[2],
			gap:     time.Duration(gap) * time.Millisecond,
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
	dialer    blecore.Dialer[bluetooth.Device]

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

// scan stops early once target has settled (see blecore.Radio.Run).
func scan(ctx context.Context, timeout time.Duration, target *blecore.Target) ([]blecore.Advert, error) {
	if err := enable(); err != nil {
		return nil, err
	}
	scanMu.Lock()
	defer scanMu.Unlock()
	// An adapter gone at the deadline (Windows reports it as the watcher stops) is still ErrNoBluetooth.
	ads, err := bleradio.Scan(ctx, adapter, timeout, target, func(err error) bool {
		return errors.Is(scanError(err), blecore.ErrNoBluetooth)
	})
	if err != nil {
		return nil, scanError(err)
	}
	return ads, nil
}

// Transport is one BLE connection to a printer.
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
	found, err := scan(ctx, openScan, &blecore.Target{Name: t.Name, Address: t.Address})
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
	}, func(err error) bool {
		return !errors.Is(err, blecore.ErrConnectTimeout) && !errors.Is(err, blecore.ErrConnectPending)
	}, func() { dropStale(ctx, addr) })
	if err != nil {
		return fmt.Errorf("ble: connect %s: %w", ad.Address, err)
	}
	if err := t.setup(dev, addr.String()); err != nil {
		_ = dev.Disconnect()
		return err
	}
	return nil
}

// connect gives up after connectTimeout. A connection that still comes up within 30 s is dropped before
// connect returns (Open holds connectMu); an attempt running longer keeps the printer's address pending
// (see blecore.Dialer), so its late cleanup can't drop a later Open's link to the same printer.
func connect(ctx context.Context, addr bluetooth.Address) (bluetooth.Device, error) {
	return dialer.Dial(ctx, addr.String(), connectTimeout, 30*time.Second, func() (bluetooth.Device, error) {
		return adapter.Connect(addr, bluetooth.ConnectionParams{ConnectionTimeout: bluetooth.NewDuration(connectTimeout)})
	}, func(d bluetooth.Device) { _ = d.Disconnect() })
}

func (t *Transport) setup(dev bluetooth.Device, addr string) error {
	tx, rx, err := bleradio.Characteristics(dev, t.service, t.write, t.notify)
	if err != nil {
		return fmt.Errorf("ble: %w", err)
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
