package usb

import (
	"io"
	"os"
)

func listNative(bool) ([]Printer, error) { return listSysfs("/sys") }

// OpenDevice opens a usblp device read/write.
func OpenDevice(device string) (io.ReadWriteCloser, error) { return os.OpenFile(device, os.O_RDWR, 0) }
