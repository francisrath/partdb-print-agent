//go:build darwin

package usb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/francisrath/pdb-print/internal/transport/ble"
)

// macOS has no printer device to open: the helper pdb-print-usb (cgo, IOUSBHost) talks to the printer-class
// interface, through the same client as the Bluetooth helper. Device is the interface's iokit:0x… id.

func listNative(withIDs bool) ([]Printer, error) {
	ps, err := ble.ListUSB(context.Background(), withIDs)
	if err != nil {
		return nil, err
	}
	out := make([]Printer, 0, len(ps))
	for _, p := range ps {
		out = append(out, Printer{Device: p.Device, USBID: p.USBID, Serial: p.Serial, IEEE1284: ParseIEEE1284(p.IEEE1284)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// openPrinter opens the printer for a job; the helper waits a few seconds when macOS's own print queue
// still has it.
func openPrinter(device string) (io.ReadWriteCloser, error) {
	t := ble.NewUSB(device)
	if err := t.Open(context.Background()); err != nil {
		return nil, err
	}
	return helperDevice{t}, nil
}

// helperDevice gives the helper connection the read deadline Transport.Read sets.
type helperDevice struct{ *ble.Transport }

func (d helperDevice) SetReadDeadline(t time.Time) error {
	d.SetReadTimeout(max(time.Until(t), time.Millisecond))
	return nil
}

// OpenDevice is the online check's open: on macOS it only checks that the printer is connected, since
// opening it would take the interface from macOS's own print queue (and start a helper) every heartbeat.
func OpenDevice(device string) (io.ReadWriteCloser, error) {
	ps, err := listNative(false)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(ps, func(p Printer) bool { return p.Device == device }) {
		return nil, fmt.Errorf("USB printer %s not connected", device)
	}
	return present{}, nil
}

// present is a printer that is there but not opened.
type present struct{}

var errNotOpened = errors.New("usb: presence check only")

func (present) Read([]byte) (int, error)  { return 0, errNotOpened }
func (present) Write([]byte) (int, error) { return 0, errNotOpened }
func (present) Close() error              { return nil }
