// Package serial is the RS-232 transport (a USB-serial adapter, a built-in COM port or a Pi's UART):
// /dev/ttyUSB0, /dev/cu.usbserial-…, COM3. The printer language is the same as over USB or the network;
// this only sets up the line (baud, framing, flow control).
package serial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
	"github.com/francisrath/pdb-print/internal/transport"
)

func init() {
	transport.Register("serial", func(c config.Transport, _ *devicedb.Model) (driver.Transport, error) {
		return &Transport{Device: c.Device, Line: Line{Baud: c.Baud, Framing: c.Framing, Flow: c.Flow}}, nil
	})
}

// Line is how the port is set up. Zero values are config's defaults: 9600, 8N1, no flow control.
type Line struct {
	Baud    int
	Framing string // 8 data bits, parity (N/E/O), stop bits: "8N1"
	Flow    string // none | xonxoff | rtscts
}

func (l Line) withDefaults() Line {
	if l.Baud == 0 {
		l.Baud = 9600
	}
	if l.Framing == "" {
		l.Framing = "8N1"
	}
	if l.Flow == "" {
		l.Flow = "none"
	}
	return l
}

// port is an open serial line; the OS files implement it.
type port interface {
	read(p []byte, timeout time.Duration) (int, error) // (0, nil) when nothing came within timeout
	write(p []byte) (int, error)
	drain(timeout time.Duration) error // wait until the OS has sent everything written
	close() error
}

type Transport struct {
	Device      string
	Line        Line
	ReadTimeout time.Duration // per Read; 0 = 2 s
	// OpenPort opens Device; nil = the OS serial port. Tests swap in a fake.
	OpenPort func(device string, l Line) (port, error)
	p        port
}

func (t *Transport) Kind() string { return "serial" }

func (t *Transport) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	open := t.OpenPort
	if open == nil {
		open = openPort
	}
	p, err := open(t.Device, t.Line.withDefaults())
	if err != nil {
		return fmt.Errorf("serial %s: %w", t.Device, err)
	}
	t.p = p
	return nil
}

// Write blocks until the OS took all of p; with flow control a busy printer holds it up.
func (t *Transport) Write(p []byte) (int, error) {
	if t.p == nil {
		return 0, os.ErrClosed
	}
	return t.p.write(p)
}

// SetReadTimeout sets ReadTimeout (driver.ReadTimeoutSetter); 0 = 2 s again.
func (t *Transport) SetReadTimeout(d time.Duration) { t.ReadTimeout = d }

var _ driver.ReadTimeoutSetter = (*Transport)(nil)

// Read returns os.ErrDeadlineExceeded when the printer says nothing within ReadTimeout. A line wired
// one way (or a printer that doesn't answer) looks the same: drivers treat silence as no read-back.
func (t *Transport) Read(p []byte) (int, error) {
	if t.p == nil {
		return 0, os.ErrClosed
	}
	to := t.ReadTimeout
	if to == 0 {
		to = 2 * time.Second
	}
	n, err := t.p.read(p, to)
	if n == 0 && err == nil {
		return 0, os.ErrDeadlineExceeded
	}
	return n, err
}

// drainTimeout bounds how long Close waits for the last bytes to leave (a printer holding XOFF or CTS
// keeps them queued).
const drainTimeout = 30 * time.Second

func (t *Transport) Close() error {
	if t.p == nil {
		return nil
	}
	err := t.p.drain(drainTimeout) // closing a port can drop what is still queued
	err = errors.Join(err, t.p.close())
	t.p = nil
	return err
}

var _ io.ReadWriteCloser = (*Transport)(nil)
