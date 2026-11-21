//go:build !linux && !windows && !darwin

package bt

import (
	"context"
	"fmt"
	"time"

	"github.com/francisrath/pdb-print/internal/blecore"
)

// Scan: Bluetooth Classic is supported on Linux, Windows and macOS only.
func Scan(context.Context, time.Duration) ([]Device, error) {
	return nil, fmt.Errorf("bt: %w: not supported on this OS", blecore.ErrNoBluetooth)
}
