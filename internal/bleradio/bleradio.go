//go:build linux || windows || (darwin && cgo)

// Package bleradio holds the Bluetooth library calls shared by the Linux/Windows transport
// (internal/transport/ble) and the macOS helper (cmd/pdb-print-ble). Enabling the adapter stays with them.
package bleradio

import (
	"context"
	"fmt"
	"time"

	"tinygo.org/x/bluetooth"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

// Scan runs blecore.Radio.Run on an enabled adapter; fatal is Radio.Fatal.
func Scan(ctx context.Context, a *bluetooth.Adapter, timeout time.Duration, target *blecore.Target, fatal func(error) bool) ([]blecore.Advert, error) {
	return blecore.Radio{
		Scan: func(found func(blecore.Advert)) error {
			return a.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
				ad := blecore.Advert{Name: r.LocalName(), Address: r.Address.String(), RSSI: int(r.RSSI)}
				for _, u := range r.ServiceUUIDs() {
					ad.Services = append(ad.Services, u.String())
				}
				found(ad)
			})
		},
		Stop:  a.StopScan,
		Fatal: fatal,
	}.Run(ctx, timeout, target)
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
