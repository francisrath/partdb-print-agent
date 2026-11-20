// Package blecore is the Bluetooth-library-free part of the BLE transport: the advert type, picking the
// configured printer from a scan, and the chunker that splits writes and obeys the printer's flow control.
// The in-process transport (Linux, Windows) and the macOS helper both use it.
package blecore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNoBluetooth wraps every "no usable Bluetooth adapter" error (none present, BlueZ not running, powered off).
var ErrNoBluetooth = errors.New("no Bluetooth adapter")

// ErrNotFound: no advert matched the configured name or address (yet).
var ErrNotFound = errors.New("BLE printer not found")

// Advert is one device seen in a scan.
type Advert struct {
	Name    string `json:"name"`
	Address string `json:"address"` // a MAC ("AA:BB:CC:DD:EE:FF") on Linux/Windows, a CoreBluetooth UUID on macOS
	RSSI    int    `json:"rssi"`
	// Services are the advertised service UUIDs in the library's canonical lowercase 128-bit form
	// ("0000ae30-0000-1000-8000-00805f9b34fb").
	Services []string `json:"services"`
}

// Pick finds the configured printer among the adverts. With both set, the address wins (the name is
// ignored); a name matching several devices is an error naming their addresses.
func Pick(found []Advert, name, address string) (Advert, error) {
	if address != "" {
		for _, a := range found {
			if strings.EqualFold(a.Address, address) {
				return a, nil
			}
		}
		return Advert{}, fmt.Errorf("%w: no device with address %s", ErrNotFound, address)
	}
	if name == "" {
		return Advert{}, errors.New("ble: set the printer's address or name")
	}
	var match []Advert
	for _, a := range found {
		if a.Name == name {
			match = append(match, a)
		}
	}
	switch len(match) {
	case 0:
		return Advert{}, fmt.Errorf("%w: no device named %q", ErrNotFound, name)
	case 1:
		return match[0], nil
	}
	addrs := make([]string, len(match))
	for i, a := range match {
		addrs[i] = a.Address
	}
	return Advert{}, fmt.Errorf("ble: %d devices are named %q (%s); set the address instead",
		len(match), name, strings.Join(addrs, ", "))
}

// ErrClosed: the connection was closed (or dropped) while Send was running.
var ErrClosed = errors.New("ble: connection closed")

// Chunker splits a write into pieces of at most Max bytes, waits Gap between pieces, and holds back while
// the printer has asked for a pause (fed in through OnNotify from the notification callback).
type Chunker struct {
	Max int // bytes per write; below 20 counts as 20 (the smallest ATT payload)
	Gap time.Duration
	// MaxPause is how long Send waits for the printer to lift a pause; 0 = 30 s. On Linux (tinygo
	// bluetooth v0.17.0) a central gets no remote-disconnect events, so a link dropped during a pause
	// surfaces only when this limit ends.
	MaxPause time.Duration
	Write    func([]byte) error

	mu     sync.Mutex
	paused bool
	closed bool
}

// Send returns once the last piece has been handed to Write.
func (c *Chunker) Send(b []byte) error {
	n := max(c.Max, 20)
	for i := 0; i < len(b); i += n {
		if i > 0 && c.Gap > 0 {
			time.Sleep(c.Gap)
		}
		if err := c.waitResume(); err != nil {
			return err
		}
		if err := c.Write(b[i:min(i+n, len(b))]); err != nil {
			return err
		}
	}
	return nil
}

// Close makes a running or later Send fail with ErrClosed.
func (c *Chunker) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *Chunker) waitResume() error {
	limit := c.MaxPause
	if limit == 0 {
		limit = 30 * time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		c.mu.Lock()
		paused, closed := c.paused, c.closed
		c.mu.Unlock()
		switch {
		case closed:
			return ErrClosed
		case !paused:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("ble: printer paused the transfer for over %v", limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// OnNotify recognises the cat printers' flow-control notification 51 78 AE 01 01 00 10 (pause) / 00
// (resume). It reports the pause state and whether this notification changed it; other notifications
// leave it alone (changed false).
func (c *Chunker) OnNotify(b []byte) (pause, changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(b) < 7 || b[0] != 0x51 || b[1] != 0x78 || b[2] != 0xAE || b[3] != 0x01 || b[4] != 0x01 || b[5] != 0x00 ||
		(b[6] != 0x10 && b[6] != 0x00) {
		return c.paused, false
	}
	p := b[6] == 0x10
	changed = p != c.paused
	c.paused = p
	return p, changed
}

// ConnectRetry runs connect, and once more when it fails with an error retry accepts and ctx is still
// live: first reset (drop a stale link), then a pause. BlueZ sometimes aborts a connect at once
// (le-connection-abort-by-local), more so right after a printer error.
func ConnectRetry(ctx context.Context, pause time.Duration, connect func() error, retry func(error) bool, reset func()) error {
	err := connect()
	if err == nil || !retry(err) || ctx.Err() != nil {
		return err
	}
	reset()
	select {
	case <-ctx.Done():
		return err
	case <-time.After(pause):
	}
	return connect()
}

// ErrConnectTimeout: Dial gave up waiting for the connection.
var ErrConnectTimeout = errors.New("no connection")

// ErrConnectPending: an earlier, abandoned attempt to the same address hasn't ended yet.
var ErrConnectPending = errors.New("previous connection attempt")

// Dialer runs connection attempts and remembers, per address, the abandoned ones still running. The zero
// value is ready to use.
type Dialer[D any] struct {
	mu      sync.Mutex
	pending map[string]bool
}

// Dial runs connect to addr, giving up after timeout or when ctx ends. An abandoned attempt may still
// connect later: Dial waits up to grace for it to end and drops what it brought up. One still running
// after grace (v0.17.0's Linux Connect can wait for its connected signal forever) keeps addr pending:
// Dials to addr fail at once until it ends, so its late cleanup can't drop a newer link to the device.
func (x *Dialer[D]) Dial(ctx context.Context, addr string, timeout, grace time.Duration, connect func() (D, error), drop func(D)) (D, error) {
	var zero D
	x.mu.Lock()
	if x.pending[addr] {
		x.mu.Unlock()
		return zero, fmt.Errorf("%w to %s still pending; switch the printer off and on", ErrConnectPending, addr)
	}
	if x.pending == nil {
		x.pending = map[string]bool{}
	}
	x.pending[addr] = true
	x.mu.Unlock()
	unpend := func() {
		x.mu.Lock()
		delete(x.pending, addr)
		x.mu.Unlock()
	}

	type result struct {
		dev D
		err error
	}
	res := make(chan result) // unbuffered: an attempt either hands its result over or sees abandoned
	abandoned, done := make(chan struct{}), make(chan struct{})
	go func() {
		d, err := connect()
		select {
		case res <- result{d, err}:
		case <-abandoned:
			if err == nil {
				drop(d)
			}
			unpend() // only now: the drop above is done
			close(done)
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-res:
		unpend()
		return r.dev, r.err
	case <-timer.C:
	case <-ctx.Done():
	}
	close(abandoned)
	select {
	case <-done:
	case <-time.After(grace):
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return zero, fmt.Errorf("%w within %v", ErrConnectTimeout, timeout)
}

// Connected checks what a library Connect returned: v0.17.0's darwin Connect returns a zero Device and no
// error when CoreBluetooth reports the failure before its timeout, and that Device panics on first use.
func Connected[D comparable](dev D, err error) error {
	var zero D
	if err == nil && dev == zero {
		return errors.New("the connection failed")
	}
	return err
}

// ErrNotEnabled: Bluetooth didn't come up in time (macOS: perhaps a permission prompt still open). Not
// ErrNoBluetooth, which means a definite "off" or "none".
var ErrNotEnabled = errors.New("bluetooth didn't become available — if macOS asked for permission, allow it and run again; otherwise check that Bluetooth is on")

// EnableWait is how long the macOS helper waits for Bluetooth to come up: long enough for someone to
// answer the first-run permission prompt (CoreBluetooth stays "unknown" while it is open).
const EnableWait = 60 * time.Second

// WaitEnable calls enable (tinygo bluetooth's darwin Adapter.Enable, which gives up after 10 s) again
// while it times out, for up to wait. A definite answer (powered off, unsupported, not allowed) wraps
// ErrNoBluetooth; still nothing after wait is a plain error, since it may be a prompt left open.
func WaitEnable(enable func() error, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := enable()
		switch {
		case err == nil:
			return nil
		case err.Error() != "timeout enabling CentralManager": // text from v0.17.0's adapter_darwin.go
			return fmt.Errorf("%w: %w", ErrNoBluetooth, err)
		case !time.Now().Before(deadline):
			return ErrNotEnabled
		}
	}
}
