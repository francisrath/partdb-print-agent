package usb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// Printer is a USB printer the Linux usblp driver has bound.
type Printer struct {
	Device   string            // /dev/usb/lp0
	USBID    string            // 04f9:209c
	Serial   string            // "" if the printer reports none
	IEEE1284 map[string]string // IEEE 1284 device ID fields: MFG, MDL, CMD, …
}

// List reads the usblp printers from sysfs ("" = /sys): class/usbmisc/lpN/device is the USB
// interface (with ieee1284_id); its parent directory is the USB device (idVendor, idProduct, serial).
func List(sysfs string) ([]Printer, error) {
	if sysfs == "" {
		if runtime.GOOS != "linux" {
			return nil, errors.New("USB printers are only supported on Linux for now")
		}
		sysfs = "/sys"
	}
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
		if err != nil {
			continue
		}
		dev := filepath.Dir(iface)
		read := func(dir, name string) string {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			return strings.TrimSpace(string(b))
		}
		out = append(out, Printer{
			Device:   "/dev/usb/" + e.Name(),
			USBID:    strings.ToLower(read(dev, "idVendor") + ":" + read(dev, "idProduct")),
			Serial:   read(dev, "serial"),
			IEEE1284: ParseIEEE1284(read(iface, "ieee1284_id")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// ParseIEEE1284 splits an IEEE 1284 device ID ("MFG:Brother;MDL:QL-810W;…") into fields.
func ParseIEEE1284(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(f, ":"); ok {
			m[strings.ToUpper(strings.TrimSpace(k))] = strings.TrimSpace(v)
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
