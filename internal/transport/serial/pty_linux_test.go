package serial

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// openPTY returns the master fd and the slave's path (the "serial port" under test).
func openPTY() (int, string, error) {
	m, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	if err := unix.IoctlSetPointerInt(m, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(m)
		return -1, "", err
	}
	n, err := unix.IoctlGetUint32(m, unix.TIOCGPTN)
	if err != nil {
		unix.Close(m)
		return -1, "", err
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}
