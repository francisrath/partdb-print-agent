//go:build linux || darwin

package serial

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Ports lists likely printer ports: on Linux USB-serial adapters and Pi UARTs, by their stable
// /dev/serial/by-id name where there is one (built-in ttyS ports always exist, so they aren't listed), on macOS the /dev/cu.* call-out devices
// except the built-in Bluetooth and debug ones.
func Ports() []string {
	if runtime.GOOS == "linux" {
		return linuxPorts("/dev")
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

// linuxPorts lists dev's serial/by-id links, then the ttyUSB, ttyACM and ttyAMA devices no link points to.
func linuxPorts(dev string) []string {
	out, _ := filepath.Glob(filepath.Join(dev, "serial/by-id/*"))
	linked := map[string]bool{}
	for _, l := range out {
		if t, err := filepath.EvalSymlinks(l); err == nil {
			linked[t] = true
		}
	}
	for _, pat := range []string{"ttyUSB*", "ttyACM*", "ttyAMA*"} {
		ps, _ := filepath.Glob(filepath.Join(dev, pat))
		for _, p := range ps {
			if t, err := filepath.EvalSymlinks(p); err != nil || !linked[t] {
				out = append(out, p)
			}
		}
	}
	return out
}
