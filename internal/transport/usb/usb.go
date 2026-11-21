// Package usb is the USB printer-class transport via the Linux usblp driver (/dev/usb/lpN).
package usb

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport"
)

func init() {
	transport.Register("usb", func(c config.Transport, _ *devicedb.Model) (driver.Transport, error) {
		return &Transport{Device: c.Device, USBID: c.USBID, Serial: c.Serial}, nil
	})
}

const pollInterval = 50 * time.Millisecond

type Transport struct {
	Device      string        // /dev/usb/lp0; looked up from USBID (+ Serial) at each Open when set
	USBID       string        // 04f9:209c
	Serial      string        // tells identical printers apart
	Sysfs       string        // "" = /sys (tests use a fake tree)
	ReadTimeout time.Duration // per Read; 0 = 2s
	// OpenFile opens Device; nil = os.OpenFile read/write. Tests swap in a fake.
	OpenFile func(device string) (io.ReadWriteCloser, error)
	f        io.ReadWriteCloser
}

func New(device string) *Transport { return &Transport{Device: device} }

func (t *Transport) Kind() string { return "usb" }

func (t *Transport) Open(ctx context.Context) error {
	open := t.OpenFile
	if open == nil {
		open = func(d string) (io.ReadWriteCloser, error) { return os.OpenFile(d, os.O_RDWR, 0) }
	}
	if t.USBID != "" { // a replugged printer may come back as another lpN
		dev, err := Find(t.Sysfs, t.USBID, t.Serial)
		if err != nil {
			return err
		}
		t.Device = dev
	}
	f, err := open(t.Device)
	if err != nil {
		return err
	}
	t.f = f
	return nil
}

func (t *Transport) Write(p []byte) (int, error) { return t.f.Write(p) }

// SetReadTimeout sets ReadTimeout (driver.ReadTimeoutSetter), e.g. to drop stale bytes quickly; 0 = 2 s again.
func (t *Transport) SetReadTimeout(d time.Duration) { t.ReadTimeout = d }

var _ driver.ReadTimeoutSetter = (*Transport)(nil)

// Read returns os.ErrDeadlineExceeded when the printer says nothing within ReadTimeout.
// usblp doesn't block: with nothing queued a read returns 0 bytes, which os.File reports
// as io.EOF (measured on a Pi 2 with a PT-9500PC, 2026-11-18). So poll until data arrives.
func (t *Transport) Read(p []byte) (int, error) {
	to := t.ReadTimeout
	if to == 0 {
		to = 2 * time.Second
	}
	deadline := time.Now().Add(to)
	if d, ok := t.f.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(deadline) // a read blocking inside usblp ends at the deadline too
	}
	for {
		n, err := t.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(pollInterval)
	}
}

func (t *Transport) Close() error {
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}
