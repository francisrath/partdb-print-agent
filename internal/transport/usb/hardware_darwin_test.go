//go:build hardware && darwin

package usb

import (
	"bytes"
	"errors"
	"os"
	"strings"
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
		if p.USBID == "" {
			t.Errorf("%s: no USB id", p.Device)
		}
		if p.IEEE1284["MFG"] == "" { // macOS's own queue had the printer just then
			t.Logf("%s: no IEEE 1284 ID (busy?)", p.Device)
		}
		if dev, err := Find("", p.USBID, p.Serial); err != nil || dev != p.Device {
			t.Errorf("Find(%s, %s) = %q, %v", p.USBID, p.Serial, dev, err)
		}
		if !strings.HasPrefix(p.USBID, "04f9:") { // Brother
			continue
		}
		tr := &Transport{USBID: p.USBID, Serial: p.Serial, ReadTimeout: 3 * time.Second}
		if err := tr.Open(t.Context()); err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() }) // a failed read below mustn't leave the helper holding the printer
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
		if _, err := tr.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > 4*time.Second {
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
