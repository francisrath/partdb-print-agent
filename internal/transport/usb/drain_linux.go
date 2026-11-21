package usb

import (
	"io"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// drain waits until usblp has handed the last write to the printer. Go opens /dev/usb/lpN non-blocking
// (it supports poll), so Write returns with the last URB (up to 8 KB) still in flight, and close kills
// it: the end of the job would be lost. usblp signals POLLOUT once that write completed (or on no paper).
func drain(f io.ReadWriteCloser, timeout time.Duration) {
	sc, ok := f.(syscall.Conn)
	if !ok {
		return
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		for {
			if _, err := unix.Poll(fds, int(timeout.Milliseconds())); err != unix.EINTR {
				return
			}
		}
	})
}
