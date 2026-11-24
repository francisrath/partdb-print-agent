//go:build !linux

package usb

import (
	"errors"
	"io"
	"os"
)

func listNative() ([]Printer, error) {
	return nil, errors.New("USB printers are only supported on Linux and Windows for now")
}

// OpenDevice opens a device path read/write.
func OpenDevice(device string) (io.ReadWriteCloser, error) { return os.OpenFile(device, os.O_RDWR, 0) }
