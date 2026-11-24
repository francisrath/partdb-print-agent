package usb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Printer is a USB printer the OS printer-class driver has bound.
type Printer struct {
	Device   string            // /dev/usb/lp0; Windows: \\?\USB#VID_…#{28d78fad-…} interface path
	USBID    string            // 04f9:209c
	Serial   string            // "" if the printer reports none
	IEEE1284 map[string]string // IEEE 1284 device ID fields: MFG, MDL, CMD, …
}

// List returns the USB printers the OS driver has bound: Linux usblp, Windows usbprint.sys.
// A non-empty sysfs reads that tree instead, on any OS (tests use fake trees).
func List(sysfs string) ([]Printer, error) {
	if sysfs == "" {
		return listNative()
	}
	return listSysfs(sysfs)
}

// listSysfs reads the usblp printers from sysfs: class/usbmisc/lpN/device is the USB
// interface (with ieee1284_id); its parent directory is the USB device (idVendor, idProduct, serial).
func listSysfs(sysfs string) ([]Printer, error) {
	entries, err := os.ReadDir(filepath.Join(sysfs, "class", "usbmisc"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // no usblp printer (or usblp not loaded)
	}
	if err != nil {
		return nil, err
	}
	var out []Printer
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "lp") {
			continue
		}
		iface, err := filepath.EvalSymlinks(filepath.Join(sysfs, "class", "usbmisc", e.Name(), "device"))
		if errors.Is(err, os.ErrNotExist) {
			continue // unplugged while we looked
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		dev := filepath.Dir(iface)
		var firstErr error
		read := func(dir, name string, optional bool) string {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil && !(optional && errors.Is(err, os.ErrNotExist)) && firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", e.Name(), err)
			}
			return strings.TrimSpace(string(b))
		}
		p := Printer{
			Device:   "/dev/usb/" + e.Name(),
			USBID:    strings.ToLower(read(dev, "idVendor", false) + ":" + read(dev, "idProduct", false)),
			Serial:   read(dev, "serial", true),
			IEEE1284: ParseIEEE1284(read(iface, "ieee1284_id", true)),
		}
		if firstErr != nil {
			return nil, firstErr // a printer we can't inspect must not be skipped silently
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// ParseIEEE1284 splits an IEEE 1284 device ID ("MFG:Brother;MDL:QL-810W;…") into fields.
// longKeys are the spelled-out IEEE 1284 keys some printers (Zebra) use instead of MFG/MDL/CMD.
var longKeys = map[string]string{"MANUFACTURER": "MFG", "MODEL": "MDL", "COMMAND SET": "CMD"}

func ParseIEEE1284(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(f, ":"); ok {
			k = strings.ToUpper(strings.TrimSpace(k))
			if short, ok := longKeys[k]; ok {
				k = short
			}
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

// Find returns the device of the printer with usbID (and serial, if given).
func Find(sysfs, usbID, serial string) (string, error) {
	ps, err := List(sysfs)
	if err != nil {
		return "", err
	}
	var hits []Printer
	for _, p := range ps {
		if p.USBID == usbID && (serial == "" || p.Serial == serial) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].Device, nil
	case 0:
		if serial != "" {
			return "", fmt.Errorf("no USB printer %s with serial %s connected", usbID, serial)
		}
		return "", fmt.Errorf("no USB printer %s connected", usbID)
	}
	var serials []string
	for _, h := range hits {
		if h.Serial != "" && !slices.Contains(serials, h.Serial) {
			serials = append(serials, h.Serial)
		}
	}
	if serial == "" && len(serials) == len(hits) {
		return "", fmt.Errorf("%d USB printers %s connected: set serial (%s)", len(hits), usbID, strings.Join(serials, ", "))
	}
	return "", fmt.Errorf("%d USB printers %s connected that serial can't tell apart: use device: /dev/usb/lpN instead", len(hits), usbID)
}
