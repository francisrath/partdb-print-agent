package bt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

func init() { register(dial) }

var linuxErrnos = errnos{
	// EACCES: authentication failed, i.e. not (or no longer) paired.
	off: []syscall.Errno{unix.EHOSTDOWN, unix.ECONNREFUSED, unix.ETIMEDOUT, unix.EACCES},
	// EHOSTUNREACH: the kernel found no powered-up adapter to route through (hci_get_route);
	// EAFNOSUPPORT: no Bluetooth support in the kernel.
	noAdapter: []syscall.Errno{unix.EADDRNOTAVAIL, unix.ENODEV, unix.EHOSTUNREACH, unix.EAFNOSUPPORT},
}

func dial(ctx context.Context, address, name string, channel int) (conn, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_RFCOMM)
	if err != nil {
		return nil, mapErr(label(address, name), err, linuxErrnos)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()
	if address == "" {
		devs, err := bluezDevices(ctx)
		if err != nil {
			return nil, err
		}
		if address, err = resolve(devs, name); err != nil {
			return nil, err
		}
	}
	a, err := ParseAddr(address)
	if err != nil {
		return nil, err
	}
	if channel == 0 {
		channel = 1
	}
	printer := label(address, name)
	err = unix.Connect(fd, &unix.SockaddrRFCOMM{Addr: rfcommAddr(a), Channel: uint8(channel)})
	if errors.Is(err, unix.EINPROGRESS) {
		err = waitConnected(ctx, fd, connectTimeout)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, mapErr(printer, err, linuxErrnos)
	}
	ok = true
	// Non-blocking, so the runtime poller serves it: deadlines work and Close interrupts a Read.
	return os.NewFile(uintptr(fd), "bt:"+address), nil
}

// waitConnected polls a non-blocking connect until it finishes, the timeout (os.ErrDeadlineExceeded)
// or ctx ends.
func waitConnected(ctx context.Context, fd int, timeout time.Duration) error {
	end := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(end)
		if left <= 0 {
			return os.ErrDeadlineExceeded
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(pfd, int(min(left, 200*time.Millisecond)/time.Millisecond)+1)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			return err
		}
		v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if v != 0 {
			return syscall.Errno(v)
		}
		return nil
	}
}

// bluezDevices lists every device BlueZ knows (paired or seen), once by Name and once by Alias.
func bluezDevices(ctx context.Context) ([]device, error) {
	bus, err := dbus.SystemBus() // shared connection: not closed here
	if err != nil {
		return nil, fmt.Errorf("bt: %w: D-Bus: %w", blecore.ErrNoBluetooth, err)
	}
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err = bus.Object("org.bluez", "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs)
	if err != nil {
		return nil, fmt.Errorf("bt: %w: BlueZ: %w", blecore.ErrNoBluetooth, err)
	}
	var devs []device
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		addr, _ := props["Address"].Value().(string)
		for _, key := range []string{"Name", "Alias"} {
			if n, ok := props[key].Value().(string); ok && addr != "" {
				devs = append(devs, device{Name: n, Address: addr})
			}
		}
	}
	return devs, nil
}
