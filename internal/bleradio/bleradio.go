//go:build linux || windows || (darwin && cgo)

// Package bleradio holds the Bluetooth library calls shared by the Linux/Windows transport
// (internal/transport/ble) and the macOS helper (cmd/pdb-print-ble). Enabling the adapter stays with them.
package bleradio

import (
	"context"
	"fmt"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

// Scan listens on an enabled adapter until timeout, ctx ends, or settled (if set) reports true, and returns
// every device seen once (by address). If the scan ends by itself with an error, that error is returned
// as the library gave it.
func Scan(ctx context.Context, a *bluetooth.Adapter, timeout time.Duration, settled func(*blecore.Scanned, time.Time) bool) ([]blecore.Advert, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		mu   sync.Mutex
		seen blecore.Scanned
	)
	errc := make(chan error, 1)
	go func() {
		errc <- a.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
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
			if err != nil {
				return nil, err
			}
			return results(), nil
		case <-ctx.Done():
			stopScan(a, errc) // an error while stopping doesn't spoil what was found
			return results(), nil
		case <-tick.C:
			mu.Lock()
			done := settled != nil && settled(&seen, time.Now())
			mu.Unlock()
			if done {
				stopScan(a, errc)
				return results(), nil
			}
		}
	}
}

// stopScan stops the scan and waits for the library's Scan to return. StopScan fails while Scan is still
// starting and may be lost on Windows before the watcher has started, so it is repeated until Scan returns.
// Once stopped, v0.17.0 returns promptly: BlueZ's loop sees the closed cancel channel at once, Windows
// waits for the watcher's Stopped event.
func stopScan(a *bluetooth.Adapter, errc <-chan error) {
	for {
		_ = a.StopScan()
		select {
		case <-errc:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Characteristics finds the write and notify characteristics of service on a connected device. The
// errors carry no package prefix; callers add their own.
func Characteristics(dev bluetooth.Device, service, write, notify bluetooth.UUID) (tx, rx *bluetooth.DeviceCharacteristic, err error) {
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{service})
	if err != nil || len(svcs) == 0 {
		return nil, nil, fmt.Errorf("printer has no service %s: %v", service, err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{write, notify})
	if err != nil {
		return nil, nil, fmt.Errorf("characteristics: %w", err)
	}
	for i := range chars {
		switch chars[i].UUID() {
		case write:
			tx = &chars[i]
		case notify:
			rx = &chars[i]
		}
	}
	if tx == nil || rx == nil {
		return nil, nil, fmt.Errorf("printer lacks characteristic %s or %s", write, notify)
	}
	return tx, rx, nil
}
