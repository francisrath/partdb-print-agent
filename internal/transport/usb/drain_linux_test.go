package usb

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestDrain(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := drain(w, time.Second); err != nil {
		t.Fatalf("empty pipe: %v", err)
	}
	// fill the pipe: nothing takes the data, like a printer that stopped reading
	rc, _ := w.SyscallConn()
	_ = rc.Control(func(fd uintptr) {
		buf := make([]byte, 4096)
		for {
			if _, err := syscall.Write(int(fd), buf); err != nil {
				return
			}
		}
	})
	if err := drain(w, 50*time.Millisecond); err == nil {
		t.Fatal("full pipe: no error")
	}
}
