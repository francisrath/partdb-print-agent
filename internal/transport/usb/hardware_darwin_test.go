//go:build hardware && darwin

package usb

import (
	"bytes"
	"testing"
	"time"
)

// TestListAndStatusOnMac needs pdb-print-usb in PATH (or next to the test binary) and a USB printer with
// readback: it lists, then asks a Brother printer for its 32-byte status (prints nothing).
func TestListAndStatusOnMac(t *testing.T) {
	ps, err := List("")
	if err != nil || len(ps) == 0 {
		t.Fatalf("List: %v %v", ps, err)
	}
	for _, p := range ps {
		t.Logf("%+v", p)
		if p.USBID == "" || p.IEEE1284["MFG"] == "" {
			t.Errorf("%s: no USB id or 1284 ID", p.Device)
		}
		if dev, err := Find("", p.USBID, p.Serial); err != nil || dev != p.Device {
			t.Errorf("Find(%s, %s) = %q, %v", p.USBID, p.Serial, dev, err)
		}
		if p.IEEE1284["MFG"] != "Brother" {
			continue
		}
		tr := &Transport{USBID: p.USBID, Serial: p.Serial, ReadTimeout: 3 * time.Second}
		if err := tr.Open(t.Context()); err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := tr.Write(append(make([]byte, 200), 0x1b, 0x40, 0x1b, 0x69, 0x53)); err != nil {
			t.Fatalf("write: %v", err)
		}
		var got []byte
		buf := make([]byte, 64)
		for len(got) < 32 {
			n, err := tr.Read(buf)
			if err != nil {
				t.Fatalf("read after %d bytes: %v", len(got), err)
			}
			got = append(got, buf[:n]...)
		}
		start := time.Now()
		if _, err := tr.Read(buf); err == nil || time.Since(start) > 4*time.Second {
			t.Errorf("idle read: %v after %v, want a timeout around 3 s", err, time.Since(start))
		}
		if err := tr.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if !bytes.HasPrefix(got, []byte{0x80, 0x20, 0x42}) {
			t.Errorf("status % x", got)
		}
		t.Logf("status % x", got)
	}
}
