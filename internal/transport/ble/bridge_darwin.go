//go:build darwin

package ble

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/blebridge"
	"github.com/francisrath/partdb-print-agent/internal/blecore"
	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/transport"
)

// On macOS pdb-print never imports the Bluetooth library (it asks for Bluetooth permission at package
// init); the helper pdb-print-ble does the talking, one helper process per open connection.

const (
	helperName  = "pdb-print-ble"
	readTimeout = 2 * time.Second
	// for the helper to disconnect and exit before it is killed: it waits for the disconnect to be confirmed
	closeGrace = blecore.NotifyOffWait + blecore.DisconnectWait + time.Second
)

var errNoHelper = errors.New("install pdb-print-ble (comes with the macOS release) for Bluetooth printers")

// errDenied: macOS privacy (TCC) kills the helper with SIGABRT when the app that started pdb-print (its
// "responsible" app, e.g. the Claude desktop app) may not use Bluetooth. Not ErrNoBluetooth: the adapter is fine.
var errDenied = errors.New("macOS stopped " + helperName + " from using Bluetooth because the app that started " +
	"pdb-print isn't allowed to (run it from Terminal or another terminal app and allow Bluetooth when asked; " +
	"a service needs its own permission)")

// killedByPrivacy reports whether the helper died of SIGABRT.
func killedByPrivacy(ps *os.ProcessState) bool {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGABRT
}

func init() {
	transport.Register("ble", func(c config.Transport, m *devicedb.Model) (driver.Transport, error) {
		q := func(key string, def int) int {
			if m == nil {
				return def
			}
			return m.Quirk(key, def)
		}
		return &Transport{req: blebridge.OpenRequest{
			Name:    c.Name,
			Address: c.Address,
			Service: uint16(q("ble_service", 0xae30)),
			Write:   uint16(q("ble_write", 0xae01)),
			Notify:  uint16(q("ble_notify", 0xae02)),
			GapMS:   q("ble_gap_ms", 10),
		}}, nil
	})
	// Bluetooth Classic (Serial Port Profile) goes through the same helper on macOS: IOBluetooth also asks
	// for Bluetooth permission. Package bt registers kind bt on the other systems.
	transport.Register("bt", func(c config.Transport, _ *devicedb.Model) (driver.Transport, error) {
		return &Transport{kind: "bt", req: blebridge.OpenRequest{
			Kind: blebridge.KindRFCOMM, Name: c.Name, Address: c.Address, Channel: c.Channel,
		}}, nil
	})
}

// helperPath finds the helper; tests replace it so they never pick up a real one.
var helperPath = findHelper

// findHelper looks next to the running executable, then in PATH.
func findHelper() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), helperName)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	if p, err := exec.LookPath(helperName); err == nil {
		return p, nil
	}
	return "", errNoHelper
}

// helperError turns the helper's error text back into an error for transport kind, keeping ErrNoBluetooth
// recognisable.
func helperError(kind, msg string) error {
	if rest, ok := strings.CutPrefix(msg, blecore.ErrNoBluetooth.Error()); ok {
		if kind == "ble" {
			return fmt.Errorf("%w%s", blecore.ErrNoBluetooth, rest)
		}
		return fmt.Errorf("%s: %w%s", kind, blecore.ErrNoBluetooth, rest)
	}
	return fmt.Errorf("%s: %s", kind, msg)
}

// Scan runs the helper's scan mode. Without a usable adapter (powered off, Bluetooth not allowed) the
// error wraps blecore.ErrNoBluetooth; Bluetooth still not up after blecore.EnableWait (perhaps a permission
// prompt left open) is a plain error. When ctx ends early the helper is interrupted and reports what it
// has seen so far.
func Scan(ctx context.Context, timeout time.Duration) ([]blecore.Advert, error) {
	return scanLines[blecore.Advert](ctx, "ble", "scan", timeout)
}

// ScanClassic lists the paired Bluetooth Classic devices and those an inquiry finds within timeout, through
// the helper's scan-classic mode; errors as for Scan.
func ScanClassic(ctx context.Context, timeout time.Duration) ([]blebridge.ClassicDevice, error) {
	return scanLines[blebridge.ClassicDevice](ctx, "bt", "scan-classic", timeout)
}

// scanLines runs a scan mode of the helper and decodes its JSON lines; errors start with kind.
func scanLines[T any](ctx context.Context, kind, mode string, timeout time.Duration) ([]T, error) {
	helper, err := helperPath()
	if err != nil {
		return nil, err
	}
	// The helper's enable may take EnableWait plus one more library Enable (10 s) before the scan starts.
	ctx, cancel := context.WithTimeout(ctx, timeout+blecore.EnableWait+15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, mode, "--timeout", timeout.String())
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// After an interrupt the helper still exits 0 with what it found, but Output reports the cancellation.
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		if cmd.ProcessState != nil && killedByPrivacy(cmd.ProcessState) {
			return nil, fmt.Errorf("%s: %w; the helper exited: %w", kind, errDenied, err)
		}
		// Interrupted while still waiting for Bluetooth, it says so (e.g. the setup wizard's deadline
		// ended a first-run permission prompt's wait).
		if msg := fatalLine(stderr.String()); msg != "" {
			return nil, helperError(kind, msg)
		}
		if ctx.Err() != nil { // it didn't stop in time (stuck in Enable): killed, nothing to report
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %s %s: %w", kind, helperName, mode, err)
	}
	var found []T
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s: %s %s output: %w", kind, helperName, mode, err)
		}
		found = append(found, v)
	}
	return found, nil
}

// fatalMark starts the helper's fatal stderr line (its other lines are log output).
const fatalMark = "error: "

// fatalLine returns the helper's fatal message without the mark, when it is the last line of stderr.
func fatalLine(s string) string {
	s = strings.TrimSpace(s)
	msg, ok := strings.CutPrefix(s[strings.LastIndexByte(s, '\n')+1:], fatalMark)
	if !ok {
		return ""
	}
	return msg
}

// Transport is one BLE connection to a cat printer (or, kind bt, one RFCOMM link), through a
// pdb-print-ble process.
type Transport struct {
	kind   string // "" = ble
	req    blebridge.OpenRequest
	readTO time.Duration // per Read; 0 = readTimeout (SetReadTimeout)

	reqMu sync.Mutex // one request (Open, Write, Close) at a time

	mu      sync.Mutex // guards everything below
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan reply    // OK/Error answers, read by the request in flight
	done    chan struct{} // closed once the helper has exited
	dead    error         // why the helper exited
	buf     []byte        // notifications not read yet
	ready   chan struct{} // signalled when buf gains bytes
}

type reply struct {
	typ     byte
	payload []byte
}

func (t *Transport) Kind() string { return cmp.Or(t.kind, "ble") }

func (t *Transport) Open(ctx context.Context) error {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	t.stop(false) // a helper left from an earlier Open (it may have died already)
	helper, err := helperPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(helper)
	tail := new(stderrTail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail) // the helper logs there; a fatal last line explains an exit
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: start %s: %w", t.Kind(), helper, err)
	}
	done := make(chan struct{})
	t.mu.Lock()
	t.cmd, t.stdin, t.done, t.dead = cmd, stdin, done, nil
	t.replies, t.buf, t.ready = make(chan reply, 1), nil, make(chan struct{}, 1)
	t.mu.Unlock()
	go t.readLoop(cmd, stdout, tail, done)

	open, _ := json.Marshal(t.req)
	if _, err := t.request(ctx, blebridge.TypeOpen, open); err != nil {
		// Cancelled: the helper is busy scanning or connecting and won't read a Close.
		t.stop(ctx.Err() != nil)
		return err
	}
	return nil
}

// readLoop delivers the helper's frames until it exits, then reaps it.
func (t *Transport) readLoop(cmd *exec.Cmd, stdout io.Reader, tail *stderrTail, done chan struct{}) {
	var err error
	for {
		var typ byte
		var p []byte
		typ, p, err = blebridge.Read(stdout)
		if err != nil {
			break
		}
		t.mu.Lock()
		if typ == blebridge.TypeNotify {
			t.buf = append(t.buf, p...)
			select {
			case t.ready <- struct{}{}:
			default:
			}
		} else {
			select {
			case t.replies <- reply{typ, p}:
			default: // nobody asked; the helper only answers requests
			}
		}
		t.mu.Unlock()
	}
	// stdout ended (cleanly or mid-frame): the helper is exiting, and its exit says why.
	exiting := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if !exiting {
		_ = cmd.Process.Kill() // garbage on stdout (version skew?): the helper can't be trusted any more
	}
	_, _ = io.Copy(io.Discard, stdout) // let Wait finish
	werr := cmd.Wait()
	msg := fatalLine(string(tail.b))
	t.mu.Lock()
	switch {
	case !exiting:
		t.dead = fmt.Errorf("%s: %s: %w", t.Kind(), helperName, err)
	case killedByPrivacy(cmd.ProcessState):
		t.dead = fmt.Errorf("%s: %w; the helper exited: %w", t.Kind(), errDenied, werr)
	case msg != "" && werr != nil:
		t.dead = fmt.Errorf("%s: %s: %s (%w)", t.Kind(), helperName, msg, werr)
	case werr != nil:
		t.dead = fmt.Errorf("%s: %s exited: %w", t.Kind(), helperName, werr)
	default:
		t.dead = fmt.Errorf("%s: %s exited", t.Kind(), helperName)
	}
	t.mu.Unlock()
	close(done)
}

// request sends a frame and waits for the helper's answer. The caller holds reqMu.
func (t *Transport) request(ctx context.Context, typ byte, payload []byte) ([]byte, error) {
	t.mu.Lock()
	stdin, replies, done := t.stdin, t.replies, t.done
	t.mu.Unlock()
	if stdin == nil {
		return nil, fmt.Errorf("%s: not open", t.Kind())
	}
	if err := blebridge.Write(stdin, typ, payload); err != nil {
		select { // a failed pipe write means the helper is gone, or about to be
		case <-done:
			return nil, t.deadErr()
		case <-time.After(closeGrace):
			return nil, fmt.Errorf("%s: %s: %w", t.Kind(), helperName, err)
		}
	}
	var r reply
	select {
	case r = <-replies:
	case <-done:
		select { // an answer sent just before the helper exited (an Open error) wins
		case r = <-replies:
		default:
			return nil, t.deadErr()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch r.typ {
	case blebridge.TypeOK:
		return r.payload, nil
	case blebridge.TypeError:
		return nil, helperError(t.Kind(), string(r.payload))
	}
	return nil, fmt.Errorf("%s: %s: unexpected reply %q", t.Kind(), helperName, r.typ)
}

func (t *Transport) deadErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

// Write returns once the helper has written the last chunk.
func (t *Transport) Write(p []byte) (int, error) {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	for i := 0; i < len(p); i += blebridge.MaxFrame {
		if _, err := t.request(context.Background(), blebridge.TypeWrite, p[i:min(i+blebridge.MaxFrame, len(p))]); err != nil {
			return i, err
		}
	}
	return len(p), nil
}

// SetReadTimeout changes Read's wait (driver.ReadTimeoutSetter); 0 = 2 s. Call it from the goroutine that reads.
func (t *Transport) SetReadTimeout(d time.Duration) { t.readTO = d }

// Read returns buffered notification bytes; with nothing for 2 s (see SetReadTimeout), an error wrapping
// os.ErrDeadlineExceeded.
func (t *Transport) Read(p []byte) (int, error) {
	to := cmp.Or(t.readTO, readTimeout)
	timer := time.NewTimer(to)
	defer timer.Stop()
	for {
		t.mu.Lock()
		if t.cmd == nil {
			t.mu.Unlock()
			return 0, fmt.Errorf("%s: not open", t.Kind())
		}
		if len(t.buf) > 0 {
			n := copy(p, t.buf)
			t.buf = t.buf[n:]
			t.mu.Unlock()
			return n, nil
		}
		ready, done := t.ready, t.done
		t.mu.Unlock()
		select {
		case <-ready:
		case <-done:
			t.mu.Lock()
			pending := len(t.buf) > 0
			t.mu.Unlock()
			if !pending {
				return 0, t.deadErr()
			}
		case <-timer.C:
			what := "notification"
			if t.Kind() == "bt" {
				what = "answer"
			}
			return 0, fmt.Errorf("%s: no %s within %v: %w", t.Kind(), what, to, os.ErrDeadlineExceeded)
		}
	}
}

// Close asks the helper to disconnect and exit, and kills it if it doesn't.
func (t *Transport) Close() error {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	t.stop(false)
	return nil
}

// stop ends the helper: politely (Close, then a grace period) or, with kill, at once.
func (t *Transport) stop(kill bool) {
	t.mu.Lock()
	cmd, stdin, done := t.cmd, t.stdin, t.done
	t.mu.Unlock()
	if cmd == nil {
		return
	}
	if kill {
		_ = cmd.Process.Kill()
	} else {
		_ = blebridge.Write(stdin, blebridge.TypeClose, nil)
	}
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(closeGrace):
		_ = cmd.Process.Kill()
		<-done
	}
	t.mu.Lock()
	t.cmd, t.stdin = nil, nil
	t.mu.Unlock()
}

// stderrTail keeps the end of the helper's stderr (exec copies it from one goroutine; read it after Wait).
type stderrTail struct{ b []byte }

func (w *stderrTail) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	if len(w.b) > 4096 {
		w.b = w.b[len(w.b)-1024:]
	}
	return len(p), nil
}
