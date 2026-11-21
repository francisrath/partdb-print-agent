package serial

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

func setSpeed(t *unix.Termios, speed uint32) {
	t.Cflag &^= unix.CBAUD
	t.Cflag |= speed
	t.Ispeed, t.Ospeed = speed, speed
}
