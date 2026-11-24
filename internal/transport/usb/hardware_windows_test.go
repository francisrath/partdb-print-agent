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
	ps, err := listNative(true)
	if err != nil || len(ps) == 0 {
		t.Fatalf("listNative: %v, %d printers", err, len(ps))
	}
	for _, p := range ps {
		t.Logf("%+v", p)
		if p.USBID == "" || p.IEEE1284["MFG"] == "" {
			t.Errorf("%s: no USB id or 1284 MFG", p.Device)
		}
	}
	quick, err := listNative(false) // what Find uses: no 1284 query, so no printer is opened
	if err != nil || len(quick) != len(ps) {
		t.Fatalf("listNative(false): %v, %d printers, want %d", err, len(quick), len(ps))
	}
	for i, p := range quick {
		if p.USBID != ps[i].USBID || p.Serial != ps[i].Serial || len(p.IEEE1284) != 0 {
			t.Errorf("listNative(false) = %+v, want %+v without IEEE1284", p, ps[i])
		}
	}
	for _, p := range ps { // a printer that answers nothing (QL-810W) keeps its read pending: the cancel path
		readOnce(t, p.Device)
	}
}

func readOnce(t *testing.T, device string) {
	f, err := OpenDevice(device)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	_ = f.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(start.Add(2 * time.Second))
	n, err := f.Read(make([]byte, 64))
	t.Logf("%s: read %d bytes, %v, after %v", device, n, err, time.Since(start))
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("read took %v, deadline was 2s", d)
	}
}
