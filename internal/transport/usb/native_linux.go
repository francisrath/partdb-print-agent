package usb

import (
	"context"
	"io"
	"os"
)

func listNative(bool) ([]Printer, error) { return listSysfs("/sys") }

func openPrinter(_ context.Context, device string) (io.ReadWriteCloser, error) {
	return OpenDevice(device)
}

// FoundIsPresent: Find's listing says nothing about whether the device opens, so the online check opens it.
const FoundIsPresent = false

// OpenDevice opens a usblp device read/write.
func OpenDevice(device string) (io.ReadWriteCloser, error) { return os.OpenFile(device, os.O_RDWR, 0) }
