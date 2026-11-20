package bt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

const (
	connectTimeout = 15 * time.Second
	readTimeout    = 2 * time.Second
	writeTimeout   = 30 * time.Second
)

// conn is an open RFCOMM link: an *os.File on Linux, a Winsock socket on Windows, a net.Pipe in tests.
// A read past its deadline fails with an error wrapping os.ErrDeadlineExceeded.
type conn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Transport is one RFCOMM connection to a paired printer.
type Transport struct {
	Address, Name string        // the address wins (see config.Transport)
	Channel       int           // RFCOMM channel; 0 = the OS default (Linux 1, Windows SDP lookup)
	ReadTimeout   time.Duration // per Read; 0 = 2s
	dial          func(ctx context.Context) (conn, error)
	conn          conn
}

func (t *Transport) Kind() string { return "bt" }

func (t *Transport) Open(ctx context.Context) error {
	if t.dial == nil {
		return errors.New("bt: not supported on this OS")
	}
	c, err := t.dial(ctx)
	if err != nil {
		return err
	}
	t.conn = c
	return nil
}

func (t *Transport) Write(p []byte) (int, error) {
	if t.conn == nil {
		return 0, errors.New("bt: not open")
	}
	if err := t.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return 0, err
	}
	return t.conn.Write(p)
}

// Read returns an error wrapping os.ErrDeadlineExceeded when the printer says nothing within ReadTimeout,
// io.EOF (or the OS error) once the link is gone.
func (t *Transport) Read(p []byte) (int, error) {
	if t.conn == nil {
		return 0, errors.New("bt: not open")
	}
	to := t.ReadTimeout
	if to == 0 {
		to = readTimeout
	}
	if err := t.conn.SetReadDeadline(time.Now().Add(to)); err != nil {
		return 0, err
	}
	n, err := t.conn.Read(p)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return n, fmt.Errorf("bt: no answer within %v: %w", to, err)
	}
	return n, err
}

func (t *Transport) Close() error {
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close()
	t.conn = nil
	return err
}

// device is one Bluetooth device the OS knows by name (BlueZ lists Name and Alias as two entries).
type device struct{ Name, Address string }

// resolve finds the address of the device called name. The same address listed twice (alias, a second
// adapter) is one device; two addresses are refused.
func resolve(devs []device, name string) (string, error) {
	if name == "" {
		return "", errors.New("bt: set the printer's address or name")
	}
	var addrs []string
	for _, d := range devs {
		if d.Name != name {
			continue
		}
		dup := false
		for _, a := range addrs {
			dup = dup || strings.EqualFold(a, d.Address)
		}
		if !dup {
			addrs = append(addrs, d.Address)
		}
	}
	switch len(addrs) {
	case 0:
		return "", fmt.Errorf("bt: this computer knows no Bluetooth device called %q – switch it on and pair it with this computer (or set address)", name)
	case 1:
		return addrs[0], nil
	}
	return "", fmt.Errorf("bt: two Bluetooth devices are called %q; set address (one of %s)", name, strings.Join(addrs, ", "))
}

// errnos classifies the OS's socket errors.
type errnos struct {
	off       []syscall.Errno // the printer did not answer: off, out of range or not paired
	noAdapter []syscall.Errno // no usable Bluetooth adapter
}

func isAny(err error, list []syscall.Errno) bool {
	for _, e := range list {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// mapErr turns a socket or connect error into one the user can act on; printer names the device.
func mapErr(printer string, err error, e errnos) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded) || isAny(err, e.off):
		return fmt.Errorf("bt: %s did not answer – is it switched on and paired with this computer? (%w)", printer, err)
	case isAny(err, e.noAdapter):
		return fmt.Errorf("bt: %w: %w", blecore.ErrNoBluetooth, err)
	}
	return fmt.Errorf("bt: connect %s: %w", printer, err)
}
