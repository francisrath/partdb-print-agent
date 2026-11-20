package blecore

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChunkSizes(t *testing.T) {
	var sizes []int
	c := &Chunker{Max: 245, Write: func(b []byte) error { sizes = append(sizes, len(b)); return nil }}
	if err := c.Send(make([]byte, 500)); err != nil {
		t.Fatal(err)
	}
	if want := []int{245, 245, 10}; !equalInts(sizes, want) {
		t.Fatalf("chunk sizes %v, want %v", sizes, want)
	}
}

func TestChunkerPausesOnFlowControl(t *testing.T) {
	pause := []byte{0x51, 0x78, 0xAE, 0x01, 0x01, 0x00, 0x10, 0x70, 0xFF}
	resume := []byte{0x51, 0x78, 0xAE, 0x01, 0x01, 0x00, 0x00, 0x00, 0xFF}
	var (
		mu  sync.Mutex
		got []byte
	)
	wrote := make(chan struct{}, 100)
	c := &Chunker{Max: 20, Gap: 30 * time.Millisecond, Write: func(b []byte) error {
		mu.Lock()
		got = append(got, b...)
		mu.Unlock()
		wrote <- struct{}{}
		return nil
	}}
	if p, changed := c.OnNotify(pause); !p || !changed {
		t.Fatalf("pause notification: pause %v changed %v", p, changed)
	}
	if p, changed := c.OnNotify(pause); !p || changed {
		t.Fatalf("repeated pause: pause %v changed %v", p, changed)
	}
	data := bytes.Repeat([]byte("0123456789ABCDEF"), 5) // 4 chunks of 20
	done := make(chan error, 1)
	go func() { done <- c.Send(data) }()
	select {
	case <-wrote:
		t.Fatal("wrote while paused")
	case <-time.After(100 * time.Millisecond):
	}
	if p, changed := c.OnNotify(resume); p || !changed {
		t.Fatalf("resume notification: pause %v changed %v", p, changed)
	}
	<-wrote // one chunk goes out, then pause mid-send
	c.OnNotify(pause)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 20 {
		t.Fatalf("%d bytes out before the pause took hold, want exactly one chunk (20)", n)
	}
	select {
	case err := <-done:
		t.Fatalf("Send returned while paused: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	c.OnNotify(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	if p, changed := c.OnNotify([]byte{0x51, 0x78, 0xA3, 0x01, 0x01, 0x00, 0x00, 0x00, 0xFF}); p || changed {
		t.Fatalf("A3 reply counted as flow control: pause %v changed %v", p, changed)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChunkerMaxPause(t *testing.T) {
	c := &Chunker{Max: 20, MaxPause: 50 * time.Millisecond, Write: func([]byte) error { return nil }}
	c.OnNotify([]byte{0x51, 0x78, 0xAE, 0x01, 0x01, 0x00, 0x10, 0x70, 0xFF})
	start := time.Now()
	err := c.Send(make([]byte, 40))
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("err %v, want a pause timeout", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("gave up after %v", d)
	}
}

func TestChunkerWriteError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	c := &Chunker{Max: 20, Write: func([]byte) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	}}
	if err := c.Send(make([]byte, 100)); !errors.Is(err, boom) {
		t.Fatalf("err %v, want boom", err)
	}
	if calls != 2 {
		t.Fatalf("%d writes, want 2 (stop at the error)", calls)
	}
}

func TestChunkerCloseUnblocksPausedSend(t *testing.T) {
	c := &Chunker{Max: 20, Write: func([]byte) error { return nil }}
	c.OnNotify([]byte{0x51, 0x78, 0xAE, 0x01, 0x01, 0x00, 0x10, 0x70, 0xFF})
	done := make(chan error, 1)
	go func() { done <- c.Send(make([]byte, 40)) }()
	time.Sleep(30 * time.Millisecond)
	c.Close()
	c.Close() // twice is fine
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send still blocked after Close")
	}
	if err := c.Send([]byte{1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close: %v, want ErrClosed", err)
	}
}
