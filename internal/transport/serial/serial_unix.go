//go:build linux || darwin

package serial

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

var speeds = map[int]uint32{
	1200: unix.B1200, 2400: unix.B2400, 4800: unix.B4800, 9600: unix.B9600, 19200: unix.B19200,
	38400: unix.B38400, 57600: unix.B57600, 115200: unix.B115200, 230400: unix.B230400,
}

type unixPort struct{ fd int }

// openPort opens the device non-blocking (a modem line without carrier would block open) and sets it
// raw: no echo, no line editing, no CR/LF mapping, reads that return what is there.
func openPort(device string, l Line) (port, error) {
	fd, err := unix.Open(device, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	p := &unixPort{fd: fd}
	if err := p.setup(l); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	_ = unix.IoctlSetInt(fd, unix.TIOCEXCL, 0) // keep other programs out while we print; best effort
	return p, nil
}

func (p *unixPort) setup(l Line) error {
	t, err := unix.IoctlGetTermios(p.fd, ioctlGetTermios)
	if err != nil {
		return fmt.Errorf("not a serial port? %w", err)
	}
	speed, ok := speeds[l.Baud]
	if !ok {
		return fmt.Errorf("baud %d not supported", l.Baud)
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL |
		unix.IXON | unix.IXOFF | unix.IXANY | unix.INPCK
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.PARODD | unix.CSTOPB | unix.CRTSCTS
	t.Cflag |= unix.CLOCAL | unix.CREAD
	t.Cflag |= unix.CS8 // config allows 8 data bits only (binary raster data)
	switch l.Framing[1] {
	case 'E':
		t.Cflag |= unix.PARENB
	case 'O':
		t.Cflag |= unix.PARENB | unix.PARODD
	}
	if l.Framing[2] == '2' {
		t.Cflag |= unix.CSTOPB
	}
	switch l.Flow {
	case "xonxoff": // the printer's XOFF stops our output; we never pause it (we read only short replies)
		t.Iflag |= unix.IXON
		t.Cc[unix.VSTART], t.Cc[unix.VSTOP] = 0x11, 0x13 // the printer's XON/XOFF, whatever the port had
	case "rtscts":
		t.Cflag |= unix.CRTSCTS
	}
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 0, 0
	setSpeed(t, speed)
	return unix.IoctlSetTermios(p.fd, ioctlSetTermios, t)
}

func (p *unixPort) read(b []byte, timeout time.Duration) (int, error) {
	if ok, err := p.wait(unix.POLLIN, timeout); !ok || err != nil {
		return 0, err
	}
	n, err := unix.Read(p.fd, b)
	if errors.Is(err, unix.EAGAIN) {
		return 0, nil
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

// stallTimeout is how long a write may make no progress (flow control holding it) before it fails.
const stallTimeout = 30 * time.Second

func (p *unixPort) write(b []byte) (int, error) {
	done := 0
	for done < len(b) {
		n, err := unix.Write(p.fd, b[done:])
		if n > 0 {
			done += n
		}
		switch {
		case errors.Is(err, unix.EAGAIN), err == nil && n == 0:
			ok, err := p.wait(unix.POLLOUT, stallTimeout)
			if err != nil {
				return done, err
			}
			if !ok {
				return done, fmt.Errorf("the printer took no data for %v (flow control holding, or wrong flow setting)", stallTimeout)
			}
		case errors.Is(err, unix.EINTR):
		case err != nil:
			return done, err
		}
	}
	return done, nil
}

// drain waits until the output queue is empty; the UART may still be shifting out its last byte.
func (p *unixPort) drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := unix.IoctlGetInt(p.fd, unix.TIOCOUTQ)
		if err != nil || n == 0 {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%d bytes still queued after %v: the end of the job may be lost", n, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *unixPort) close() error { return unix.Close(p.fd) }

// wait polls for events; false when the timeout passed first. A hangup (adapter unplugged) is an error.
func (p *unixPort) wait(events int16, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		fds := []unix.PollFd{{Fd: int32(p.fd), Events: events}}
		ms := int(time.Until(deadline).Milliseconds())
		if ms < 0 {
			ms = 0
		}
		n, err := unix.Poll(fds, ms)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return false, err
		case n == 0:
			return false, nil
		case fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 && fds[0].Revents&events == 0:
			return false, fmt.Errorf("serial port gone (poll events %#x)", fds[0].Revents)
		}
		return true, nil
	}
}
