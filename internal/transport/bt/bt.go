package bt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
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

	mu   sync.Mutex // guards conn only: Close must be able to interrupt a blocked Read
	conn conn
}

func (t *Transport) open() (conn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil, errors.New("bt: not open")
	}
	return t.conn, nil
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
	t.mu.Lock()
	t.conn = c
	t.mu.Unlock()
	return nil
}

func (t *Transport) Write(p []byte) (int, error) {
	c, err := t.open()
	if err != nil {
		return 0, err
	}
	if err := c.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return 0, err
	}
	return c.Write(p)
}

// Read returns an error wrapping os.ErrDeadlineExceeded when the printer says nothing within ReadTimeout,
// io.EOF (or the OS error) once the link is gone.
func (t *Transport) Read(p []byte) (int, error) {
	c, err := t.open()
	if err != nil {
		return 0, err
	}
	to := t.ReadTimeout
	if to == 0 {
		to = readTimeout
	}
	if err := c.SetReadDeadline(time.Now().Add(to)); err != nil {
		return 0, err
	}
	n, err := c.Read(p)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return n, fmt.Errorf("bt: no answer within %v: %w", to, err)
	}
	return n, err
}

func (t *Transport) Close() error {
	t.mu.Lock()
	c := t.conn
	t.conn = nil
	t.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close()
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
	noSPP     []syscall.Errno // the device answered but offers no serial port service
	retry     []syscall.Errno // a connect worth one more try (the stack still cleaning up the last link)
	noDevices []syscall.Errno // the paired-device lookup found nothing (not a failure)
}

// connectRetry runs connect once more after pause when it fails with a retryable errno. Never after
// os.ErrDeadlineExceeded: the full connect timeout has passed already.
func connectRetry(ctx context.Context, pause time.Duration, e errnos, connect func() error) error {
	return blecore.ConnectRetry(ctx, pause, connect, func(err error) bool {
		return !errors.Is(err, os.ErrDeadlineExceeded) && isAny(err, e.retry)
	}, func() {})
}

// lookupErr turns a failed paired-device lookup into an empty list (nothing remembered) or an error
// wrapping blecore.ErrNoBluetooth.
func lookupErr(err error, e errnos) ([]device, error) {
	if isAny(err, e.noDevices) {
		return nil, nil
	}
	return nil, fmt.Errorf("bt: %w: looking up paired devices: %w", blecore.ErrNoBluetooth, err)
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
	case isAny(err, e.noSPP):
		return fmt.Errorf("bt: %s has no serial port (SPP) service – is it the printer, and is it switched on? (%w)", printer, err)
	case isAny(err, e.noAdapter):
		return fmt.Errorf("bt: %w: %w", blecore.ErrNoBluetooth, err)
	}
	return fmt.Errorf("bt: connect %s: %w", printer, err)
}
