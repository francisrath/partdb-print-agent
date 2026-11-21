//go:build linux || darwin

package serial

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakePrinter is the pty master: what the transport writes to the slave arrives here.
func fakePrinter(t *testing.T) (*os.File, string) {
	t.Helper()
	m, name, err := openPTY()
	if err != nil {
		t.Skipf("no pty here: %v", err)
	}
	f := os.NewFile(uintptr(m), "ptmx")
	t.Cleanup(func() { f.Close() })
	return f, name
}

// readN reads n bytes from the master, failing after a second.
func readN(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	got := make([]byte, 0, n)
	buf := make([]byte, 4096)
	deadline := time.Now().Add(time.Second)
	for len(got) < n && time.Now().Before(deadline) {
		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
		if k, _ := unix.Poll(fds, 50); k == 0 {
			continue
		}
		k, err := unix.Read(int(f.Fd()), buf)
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			t.Error(err) // may run in a goroutine: no Fatal
			break
		}
		got = append(got, buf[:max(k, 0)]...)
	}
	return got
}

func open(t *testing.T, dev string, l Line) *Transport {
	t.Helper()
	tr := &Transport{Device: dev, Line: l}
	if err := tr.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

// Graphics are binary: newlines, CRs and XON/XOFF bytes must arrive unchanged (raw mode).
func TestWriteIsRaw(t *testing.T) {
	for _, l := range []Line{{}, {Baud: 115200, Framing: "7E2"}, {Baud: 19200, Framing: "8O1", Flow: "rtscts"}} {
		m, dev := fakePrinter(t)
		tr := open(t, dev, l)
		job := []byte("GW0,0,2,3,\x00\n\r\x11\x13\xff\r\nP1\n")
		got := make(chan []byte, 1)
		go func() { got <- readN(t, m, len(job)) }() // the printer reads while we write: Close waits for it
		if n, err := tr.Write(job); err != nil || n != len(job) {
			t.Fatalf("%+v: write %d, %v", l, n, err)
		}
		if err := tr.Close(); err != nil {
			t.Fatalf("%+v: close: %v", l, err)
		}
		if got := <-got; !bytes.Equal(got, job) {
			t.Errorf("%+v: printer got % x, want % x", l, got, job)
		}
	}
}

func TestRead(t *testing.T) {
	m, dev := fakePrinter(t)
	tr := open(t, dev, Line{})
	tr.SetReadTimeout(100 * time.Millisecond)
	start := time.Now()
	if _, err := tr.Read(make([]byte, 16)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent printer: %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > time.Second {
		t.Errorf("silent read took %v, want about 100 ms", d)
	}
	if _, err := m.Write([]byte("00\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := tr.Read(buf)
	if err != nil || string(buf[:n]) != "00\r\n" {
		t.Fatalf("read %q, %v; want the reply unchanged", buf[:n], err)
	}
}

// With xonxoff the printer's XOFF holds our output until XON.
func TestXonXoff(t *testing.T) {
	m, dev := fakePrinter(t)
	tr := open(t, dev, Line{Flow: "xonxoff"})
	if _, err := m.Write([]byte{0x13}); err != nil { // XOFF
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	wrote := make(chan error, 1)
	go func() { _, err := tr.Write([]byte("N\n")); wrote <- err }() // Linux blocks the write until XON
	if got := readN(t, m, 1); len(got) != 0 {
		t.Fatalf("printer got %q while it said XOFF", got)
	}
	if _, err := m.Write([]byte{0x11}); err != nil { // XON
		t.Fatal(err)
	}
	if got := readN(t, m, 2); string(got) != "N\n" {
		t.Fatalf("after XON the printer got %q, want %q", got, "N\n")
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}

func TestOpenErrors(t *testing.T) {
	_, dev := fakePrinter(t)
	for _, c := range []struct {
		dev string
		l   Line
	}{
		{"/dev/null", Line{}},            // not a tty
		{"/nonexistent/ttyUSB9", Line{}}, // missing adapter
		{dev, Line{Baud: 12345}},         // config validates this; the transport refuses it too
	} {
		tr := &Transport{Device: c.dev, Line: c.l}
		if err := tr.Open(context.Background()); err == nil {
			tr.Close()
			t.Errorf("%s %+v: opened, want an error", c.dev, c.l)
		}
	}
}

func TestClosedTransport(t *testing.T) {
	tr := &Transport{Device: "/dev/null"}
	if _, err := tr.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("write before open: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("close before open: %v", err)
	}
}
