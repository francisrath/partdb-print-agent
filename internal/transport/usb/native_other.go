//go:build !linux && !windows

package usb

import (
	"errors"
	"io"
	"os"
)

func listNative(bool) ([]Printer, error) {
	return nil, errors.New("USB printers are only supported on Linux and Windows for now")
}

func openPrinter(device string) (io.ReadWriteCloser, error) { return OpenDevice(device) }

// OpenDevice opens a device path read/write.
func OpenDevice(device string) (io.ReadWriteCloser, error) { return os.OpenFile(device, os.O_RDWR, 0) }
