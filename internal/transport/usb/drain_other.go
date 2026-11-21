//go:build !linux

package usb

import (
	"io"
	"time"
)

// drain is Linux only: usblp is a Linux driver.
func drain(io.ReadWriteCloser, time.Duration) {}
