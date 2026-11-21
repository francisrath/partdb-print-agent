//go:build linux || darwin

package serial

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Ports lists likely printer ports: on Linux the stable /dev/serial/by-id names of USB-serial adapters
// (built-in ttyS ports always exist, so they aren't listed), on macOS the /dev/cu.* call-out devices
// except the built-in Bluetooth and debug ones.
func Ports() []string {
	if runtime.GOOS == "linux" {
		ps, _ := filepath.Glob("/dev/serial/by-id/*")
		return ps
	}
	ps, _ := filepath.Glob("/dev/cu.*")
	out := ps[:0]
	for _, p := range ps {
		if !strings.Contains(p, "Bluetooth-Incoming-Port") && !strings.Contains(p, "debug-console") {
			out = append(out, p)
		}
	}
	return out
}

// Present: the device node exists (the adapter is plugged in).
func Present(device string) bool {
	_, err := os.Stat(device)
	return err == nil
}
