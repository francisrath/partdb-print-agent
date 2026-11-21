package usb

import (
	"fmt"
	"io"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// drain waits until usblp has handed the last write to the printer. Go opens /dev/usb/lpN non-blocking
// (it supports poll), so Write returns with the last URB (up to 8 KB) still in flight, and close kills
// it: the end of the job would be lost. usblp signals POLLOUT once that write completed (or on "no
// paper", which it only sets for a writer that asked for LPABORT: never here). An error means the last
// write may not have reached the printer.
func drain(f io.ReadWriteCloser, timeout time.Duration) error {
	sc, ok := f.(syscall.Conn)
	if !ok {
		return nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var perr error
	if err := rc.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		for {
			n, err := unix.Poll(fds, int(timeout.Milliseconds()))
			switch {
			case err == unix.EINTR:
				continue
			case err != nil:
				perr = err
			case n == 0:
				perr = fmt.Errorf("the printer took no data for %v: the end of the job may be lost", timeout)
			}
			return
		}
	}); err != nil {
		return err
	}
	return perr
}
