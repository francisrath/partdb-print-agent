package bt

import (
	"context"
	"time"

	"github.com/francisrath/pdb-print/internal/transport/ble"
)

// Scan lists the paired Bluetooth Classic devices and those an inquiry of timeout finds, through the
// helper pdb-print-ble (see ble.ScanClassic). macOS doesn't say which offer SPP.
func Scan(ctx context.Context, timeout time.Duration) ([]Device, error) {
	cds, err := ble.ScanClassic(ctx, timeout)
	if err != nil {
		return nil, err
	}
	devs := make([]Device, 0, len(cds))
	for _, d := range cds {
		devs = append(devs, Device{Name: d.Name, Address: d.Address, Class: d.Class})
	}
	return devs, nil
}
