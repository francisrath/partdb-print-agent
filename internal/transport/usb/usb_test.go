package usb

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/driver"
)

// fakeLP behaves like Linux usblp as measured on a Pi with a PT-9500PC: a read with
// nothing queued returns 0 bytes (io.EOF from os.File) instead of blocking.
type fakeLP struct {
	mu      sync.Mutex
	queue   []byte
	written bytes.Buffer
	readErr error
}

func (f *fakeLP) push(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, b...)
}

func (f *fakeLP) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(f.queue) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.queue)
	f.queue = f.queue[n:]
	return n, nil
}

func (f *fakeLP) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.Write(p)
}

func (f *fakeLP) Close() error { return nil }

func open(t *testing.T, f *fakeLP, timeout time.Duration) *Transport {
	t.Helper()
	tr := New("/dev/usb/lp0")
	tr.ReadTimeout = timeout
	tr.OpenFile = func(string) (io.ReadWriteCloser, error) { return f, nil }
	if err := tr.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestReadWaitsForData(t *testing.T) {
	f := &fakeLP{}
	tr := open(t, f, 2*time.Second)
	want := bytes.Repeat([]byte{0x80}, 32)
	time.AfterFunc(120*time.Millisecond, func() { f.push(want) })
	got := make([]byte, 32)
	if _, err := io.ReadFull(tr, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x", got)
	}
}

func TestReadTimesOut(t *testing.T) {
	tr := open(t, &fakeLP{}, 200*time.Millisecond)
	start := time.Now()
	_, err := tr.Read(make([]byte, 32))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > time.Second {
		t.Fatalf("returned after %v", d)
	}
}

func TestReadPassesRealErrors(t *testing.T) {
	tr := open(t, &fakeLP{readErr: syscall.ENODEV}, time.Second)
	if _, err := tr.Read(make([]byte, 32)); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("err = %v, want ENODEV", err)
	}
}

func TestReadUnidirectionalIsNoReadback(t *testing.T) {
	tr := open(t, &fakeLP{readErr: &os.PathError{Op: "read", Path: "/dev/usb/lp0", Err: syscall.EINVAL}}, time.Second)
	if _, err := tr.Read(make([]byte, 32)); !errors.Is(err, driver.ErrNoReadback) {
		t.Fatalf("err = %v, want ErrNoReadback", err)
	}
}

func TestWritePassesThrough(t *testing.T) {
	f := &fakeLP{}
	tr := open(t, f, time.Second)
	if _, err := tr.Write([]byte{0x1b, 'i', 'S'}); err != nil {
		t.Fatal(err)
	}
	if got := f.written.Bytes(); !bytes.Equal(got, []byte{0x1b, 'i', 'S'}) {
		t.Fatalf("written % x", got)
	}
	if tr.Kind() != "usb" {
		t.Fatalf("kind %q", tr.Kind())
	}
	if err := New("/dev/usb/lp9").Close(); err != nil {
		t.Fatalf("close unopened: %v", err)
	}
}

// blockingLP is a usblp device whose read blocks (no O_NONBLOCK) until its deadline.
type blockingLP struct {
	fakeLP
	mu       sync.Mutex
	deadline time.Time
}

func (b *blockingLP) SetReadDeadline(t time.Time) error {
	b.mu.Lock()
	b.deadline = t
	b.mu.Unlock()
	return nil
}

func (b *blockingLP) Read([]byte) (int, error) {
	b.mu.Lock()
	d := b.deadline
	b.mu.Unlock()
	if d.IsZero() {
		time.Sleep(10 * time.Second) // would hang without a deadline
	}
	time.Sleep(time.Until(d))
	return 0, os.ErrDeadlineExceeded
}

// A read that blocks inside the device is bounded by the transport's deadline too.
func TestReadDeadlineReachesTheDevice(t *testing.T) {
	tr := New("/dev/usb/lp0")
	tr.ReadTimeout = 200 * time.Millisecond
	tr.OpenFile = func(string) (io.ReadWriteCloser, error) { return &blockingLP{}, nil }
	must(t, tr.Open(t.Context()))
	start := time.Now()
	if _, err := tr.Read(make([]byte, 32)); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > time.Second {
		t.Errorf("err %v after %v", err, time.Since(start))
	}
}
