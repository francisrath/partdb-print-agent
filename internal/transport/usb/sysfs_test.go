package usb

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfs builds a /sys tree laid out like a Raspberry Pi's: class/usbmisc/lpN/device is a
// symlink to the USB interface (with ieee1284_id); its parent holds idVendor/idProduct/serial.
func fakeSysfs(t *testing.T, printers map[string][4]string) string {
	t.Helper()
	root := t.TempDir()
	for lp, p := range printers { // p = vendor, product, serial, ieee1284
		dev := filepath.Join(root, "devices", "usb1", "1-1", "port-"+lp)
		iface := filepath.Join(dev, "1-1.1.0") // real name 1-1:1.0; ":" is invalid on Windows
		must(t, os.MkdirAll(iface, 0o755))
		must(t, os.WriteFile(filepath.Join(dev, "idVendor"), []byte(p[0]+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(dev, "idProduct"), []byte(p[1]+"\n"), 0o644))
		if p[2] != "" {
			must(t, os.WriteFile(filepath.Join(dev, "serial"), []byte(p[2]+"\n"), 0o644))
		}
		must(t, os.WriteFile(filepath.Join(iface, "ieee1284_id"), []byte(p[3]), 0o644))
		class := filepath.Join(root, "class", "usbmisc", lp)
		must(t, os.MkdirAll(class, 0o755))
		must(t, os.Symlink(iface, filepath.Join(class, "device")))
	}
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const (
	qlID = "MFG:Brother;CMD:PT-CBP,URF;MDL:QL-810W;CLS:PRINTER;"
	ptID = "MFG:Brother;CMD:PT-CBP;MDL:PT-9500PC;CLS:PRINTER;"
)

func TestList(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{
		"lp0": {"04f9", "209c", "000G5G313834", qlID},
		"lp1": {"04f9", "200f", "D7G751568", ptID},
	})
	ps, err := List(sys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Device != "/dev/usb/lp0" || ps[0].USBID != "04f9:209c" || ps[0].Serial != "000G5G313834" ||
		ps[0].IEEE1284["MDL"] != "QL-810W" || ps[1].IEEE1284["MFG"] != "Brother" || ps[1].USBID != "04f9:200f" {
		t.Errorf("%+v", ps)
	}
}

func TestFind(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{
		"lp0": {"04f9", "209c", "AAA", qlID},
		"lp1": {"04f9", "200f", "", ptID},
		"lp2": {"04f9", "209c", "BBB", qlID},
	})
	for _, tc := range []struct{ id, serial, want string }{
		{"04f9:200f", "", "/dev/usb/lp1"},
		{"04f9:209c", "BBB", "/dev/usb/lp2"},
		{"04f9:209c", "AAA", "/dev/usb/lp0"},
	} {
		if got, err := Find(sys, tc.id, tc.serial); err != nil || got != tc.want {
			t.Errorf("%s %s: %q %v", tc.id, tc.serial, got, err)
		}
	}
}

// Two identical printers and no serial: refuse to guess.
func TestFindAmbiguous(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{"lp0": {"04f9", "209c", "AAA", qlID}, "lp1": {"04f9", "209c", "BBB", qlID}})
	if _, err := Find(sys, "04f9:209c", ""); err == nil || !strings.Contains(err.Error(), "serial") || !strings.Contains(err.Error(), "AAA") {
		t.Errorf("err = %v, want it to ask for a serial and list them", err)
	}
}

func TestFindMissing(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{"lp0": {"04f9", "200f", "", ptID}})
	if _, err := Find(sys, "04f9:209c", ""); err == nil || !strings.Contains(err.Error(), "04f9:209c") {
		t.Errorf("err = %v", err)
	}
}

// A replugged printer may come back as another lpN: Open looks it up every time.
func TestOpenResolvesEachTime(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{"lp0": {"04f9", "200f", "", ptID}})
	var opened []string
	tr := &Transport{USBID: "04f9:200f", Sysfs: sys, OpenFile: func(d string) (io.ReadWriteCloser, error) {
		opened = append(opened, d)
		return &fakeLP{}, nil
	}}
	must(t, tr.Open(t.Context()))
	must(t, tr.Close())
	must(t, os.Rename(filepath.Join(sys, "class", "usbmisc", "lp0"), filepath.Join(sys, "class", "usbmisc", "lp3")))
	must(t, tr.Open(t.Context()))
	if strings.Join(opened, " ") != "/dev/usb/lp0 /dev/usb/lp3" {
		t.Errorf("opened %v", opened)
	}
}

func TestFindAmbiguousWithoutSerials(t *testing.T) {
	sys := fakeSysfs(t, map[string][4]string{"lp0": {"04f9", "200f", "", ptID}, "lp1": {"04f9", "200f", "", ptID}})
	if _, err := Find(sys, "04f9:200f", ""); err == nil || !strings.Contains(err.Error(), "device:") || strings.Contains(err.Error(), "(, )") {
		t.Errorf("err = %v, want advice to use device:", err)
	}
	sys = fakeSysfs(t, map[string][4]string{"lp0": {"04f9", "200f", "X", ptID}, "lp1": {"04f9", "200f", "X", ptID}})
	if _, err := Find(sys, "04f9:200f", "X"); err == nil || strings.Contains(err.Error(), "set serial") {
		t.Errorf("err = %v", err)
	}
}
