//go:build darwin

package ble

import (
	"bufio"
	"bytes"
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
}

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

// helperError turns the helper's error text back into an error, keeping ErrNoBluetooth recognisable.
func helperError(msg string) error {
	if rest, ok := strings.CutPrefix(msg, blecore.ErrNoBluetooth.Error()); ok {
		return fmt.Errorf("%w%s", blecore.ErrNoBluetooth, rest)
	}
	return fmt.Errorf("ble: %s", msg)
}

// Scan runs the helper's scan mode. Without a usable adapter (powered off, Bluetooth not allowed) the
// error wraps blecore.ErrNoBluetooth; Bluetooth still not up after blecore.EnableWait (perhaps a permission
// prompt left open) is a plain error. When ctx ends early the helper is interrupted and reports what it
// has seen so far.
func Scan(ctx context.Context, timeout time.Duration) ([]blecore.Advert, error) {
	helper, err := findHelper()
	if err != nil {
		return nil, err
	}
	// The helper's enable may take EnableWait plus one more library Enable (10 s) before the scan starts.
	ctx, cancel := context.WithTimeout(ctx, timeout+blecore.EnableWait+15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "scan", "--timeout", timeout.String())
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// After an interrupt the helper still exits 0 with what it found, but Output reports the cancellation.
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		if cmd.ProcessState != nil && killedByPrivacy(cmd.ProcessState) {
			return nil, fmt.Errorf("ble: %w (%w)", errDenied, err)
		}
		// Interrupted while still waiting for Bluetooth, it says so (e.g. the setup wizard's deadline
		// ended a first-run permission prompt's wait).
		if msg := fatalLine(stderr.String()); msg != "" {
			return nil, helperError(msg)
		}
		if ctx.Err() != nil { // it didn't stop in time (stuck in Enable): killed, nothing to report
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ble: %s scan: %w", helperName, err)
	}
	var ads []blecore.Advert
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var ad blecore.Advert
		if err := json.Unmarshal(sc.Bytes(), &ad); err != nil {
			return nil, fmt.Errorf("ble: %s scan output: %w", helperName, err)
		}
		ads = append(ads, ad)
	}
	return ads, nil
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

// Transport is one BLE connection to a cat printer, through a pdb-print-ble process.
type Transport struct {
	req blebridge.OpenRequest

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

func (t *Transport) Kind() string { return "ble" }

func (t *Transport) Open(ctx context.Context) error {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	t.stop(false) // a helper left from an earlier Open (it may have died already)
	helper, err := findHelper()
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
		return fmt.Errorf("ble: start %s: %w", helper, err)
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
		t.dead = fmt.Errorf("ble: %s: %w", helperName, err)
	case killedByPrivacy(cmd.ProcessState):
		t.dead = fmt.Errorf("ble: %w (%w)", errDenied, werr)
	case msg != "" && werr != nil:
		t.dead = fmt.Errorf("ble: %s: %s (%w)", helperName, msg, werr)
	case werr != nil:
		t.dead = fmt.Errorf("ble: %s exited: %w", helperName, werr)
	default:
		t.dead = fmt.Errorf("ble: %s exited", helperName)
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
		return nil, errors.New("ble: not open")
	}
	if err := blebridge.Write(stdin, typ, payload); err != nil {
		select { // a failed pipe write means the helper is gone, or about to be
		case <-done:
			return nil, t.deadErr()
		case <-time.After(closeGrace):
			return nil, fmt.Errorf("ble: %s: %w", helperName, err)
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
	if r.typ == blebridge.TypeError {
		return nil, helperError(string(r.payload))
	}
	return r.payload, nil
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

// Read returns buffered notification bytes; with nothing for 2 s, an error wrapping os.ErrDeadlineExceeded.
func (t *Transport) Read(p []byte) (int, error) {
	timer := time.NewTimer(readTimeout)
	defer timer.Stop()
	for {
		t.mu.Lock()
		if t.cmd == nil {
			t.mu.Unlock()
			return 0, errors.New("ble: not open")
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
			return 0, fmt.Errorf("ble: no notification within %v: %w", readTimeout, os.ErrDeadlineExceeded)
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
