//go:build linux || darwin

package serial

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLinuxPorts(t *testing.T) {
	dev := t.TempDir()
	for _, f := range []string{"ttyUSB0", "ttyUSB1", "ttyAMA0", "ttyS0"} {
		if err := os.WriteFile(filepath.Join(dev, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dev, "serial/by-id"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dev, "serial/by-id/usb-FTDI_FT232R-if00-port0")
	if err := os.Symlink("../../ttyUSB0", link); err != nil {
		t.Fatal(err)
	}
	got := linuxPorts(dev)
	want := []string{link, filepath.Join(dev, "ttyUSB1"), filepath.Join(dev, "ttyAMA0")} // ttyUSB0 once, by its link; no ttyS
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
