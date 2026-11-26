//go:build !linux && !windows && !darwin

package usb

import (
	"context"
	"errors"
	"io"
	"os"
)

func listNative(bool) ([]Printer, error) {
	return nil, errors.New("USB printers are only supported on Linux, Windows and macOS for now")
}

func openPrinter(_ context.Context, device string) (io.ReadWriteCloser, error) {
	return OpenDevice(device)
}

// FoundIsPresent: Find's listing says nothing about whether the device opens, so the online check opens it.
const FoundIsPresent = false

// OpenDevice opens a device path read/write.
func OpenDevice(device string) (io.ReadWriteCloser, error) { return os.OpenFile(device, os.O_RDWR, 0) }
