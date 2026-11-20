package ble

import (
	"fmt"
	"strings"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

// scanError maps a scan that ended on its own to ErrNoBluetooth when the adapter is unusable. The library's
// errors are unexported, so they are matched by message – texts from tinygo.org/x/bluetooth v0.17.0;
// recheck them on a library bump:
//   - Linux: "bluetooth: adaptor is not powered" (gap_linux.go:50, errAdaptorNotPowered).
//   - Windows: Enable succeeds without a radio; the watcher's Stopped event then fails the scan with
//     "failed to stop scanning (error code N)" (gap_windows.go:198-205), N a WinRT BluetoothError:
//     1 RadioNotAvailable, 5 DisabledByPolicy, 6 NotSupported, 7 DisabledByUser.
func scanError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "bluetooth: adaptor is not powered") {
		return fmt.Errorf("%w: %w", blecore.ErrNoBluetooth, err)
	}
	var code int
	if n, _ := fmt.Sscanf(msg, "failed to stop scanning (error code %d)", &code); n == 1 {
		switch code {
		case 1, 5, 6, 7:
			return fmt.Errorf("%w: %w", blecore.ErrNoBluetooth, err)
		}
	}
	return fmt.Errorf("ble: scan: %w", err)
}
