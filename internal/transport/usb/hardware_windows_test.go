//go:build hardware && windows

package usb

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestListAndReadOnWindows needs a USB printer on usbprint.sys. It sends nothing: it lists the
// printers and checks that a read on the first one ends by its deadline (bytes or a timeout).
func TestListAndReadOnWindows(t *testing.T) {
	ps, err := listNative()
	if err != nil || len(ps) == 0 {
		t.Fatalf("listNative: %v, %d printers", err, len(ps))
	}
	for _, p := range ps {
		t.Logf("%+v", p)
		if p.USBID == "" || p.IEEE1284["MFG"] == "" {
			t.Errorf("%s: no USB id or 1284 MFG", p.Device)
		}
	}
	f, err := OpenDevice(ps[0].Device)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	_ = f.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(start.Add(2 * time.Second))
	_, err = f.Read(make([]byte, 64))
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("read took %v, deadline was 2s", d)
	}
}
