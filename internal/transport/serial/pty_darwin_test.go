package serial

import (
	"bytes"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY returns the master fd and the slave's path (the "serial port" under test).
func openPTY() (int, string, error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	fail := func(err error) (int, string, error) { unix.Close(m); return -1, "", err }
	if err := unix.IoctlSetInt(m, unix.TIOCPTYGRANT, 0); err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetInt(m, unix.TIOCPTYUNLK, 0); err != nil {
		return fail(err)
	}
	var name [128]byte
	//lint:ignore SA1019 x/sys has no wrapper for TIOCPTYGNAME; test-only
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(m), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		return fail(e)
	}
	return m, string(name[:bytes.IndexByte(name[:], 0)]), nil
}
