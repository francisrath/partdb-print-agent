package serial

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

func setSpeed(t *unix.Termios, speed uint32) { t.Ispeed, t.Ospeed = uint64(speed), uint64(speed) }
