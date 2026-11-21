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

	"github.com/francisrath/pdb-print/internal/blecore"
)

func init() { register(dial) }

// retryPause is the wait before the one connect retry.
const retryPause = 2 * time.Second

var linuxErrnos = errnos{
	// EACCES: authentication failed, i.e. not (or no longer) paired. ENOMEM/EBUSY: the kernel still
	// tears down the last link (seen right after a failed attempt); after the retry they mean the same.
	off: []syscall.Errno{unix.EHOSTDOWN, unix.ECONNREFUSED, unix.ETIMEDOUT, unix.EACCES, unix.ENOMEM, unix.EBUSY},
	// EHOSTUNREACH: the kernel found no powered-up adapter to route through (hci_get_route);
	// EAFNOSUPPORT: no Bluetooth support in the kernel.
	noAdapter: []syscall.Errno{unix.EADDRNOTAVAIL, unix.ENODEV, unix.EHOSTUNREACH, unix.EAFNOSUPPORT},
	retry:     []syscall.Errno{unix.ENOMEM, unix.EBUSY},
}

func dial(ctx context.Context, address, name string, channel int) (conn, error) {
	if address == "" {
		devs, err := bluezDevices(ctx)
		if err != nil {
			return nil, err
		}
		if address, err = Resolve(devs, name); err != nil {
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
	sa := &unix.SockaddrRFCOMM{Addr: rfcommAddr(a), Channel: uint8(channel)}
	var f *os.File
	err = connectRetry(ctx, retryPause, linuxErrnos, func() (err error) {
		f, err = connectOnce(ctx, sa, address)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, mapErr(label(address, name), err, linuxErrnos)
	}
	return f, nil
}

// connectOnce connects a new RFCOMM socket, giving up after connectTimeout (os.ErrDeadlineExceeded).
func connectOnce(ctx context.Context, sa *unix.SockaddrRFCOMM, address string) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.BTPROTO_RFCOMM)
	if err != nil {
		return nil, err
	}
	err = unix.Connect(fd, sa)
	if errors.Is(err, unix.EINPROGRESS) {
		err = waitConnected(ctx, fd, connectTimeout)
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
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
func bluezDevices(ctx context.Context) ([]Device, error) {
	bus, err := dbus.SystemBus() // shared connection: not closed here
	if err != nil {
		return nil, bluezErr(ctx, "D-Bus", err)
	}
	objs, err := managedObjects(ctx, bus)
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		addr, _ := props["Address"].Value().(string)
		for _, key := range []string{"Name", "Alias"} {
			if n, ok := props[key].Value().(string); ok && addr != "" {
				devs = append(devs, Device{Name: n, Address: addr})
			}
		}
	}
	return devs, nil
}

type bluezObjects = map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func managedObjects(ctx context.Context, bus *dbus.Conn) (bluezObjects, error) {
	var objs bluezObjects
	err := bus.Object("org.bluez", "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs)
	if err != nil {
		return nil, bluezErr(ctx, "BlueZ", err)
	}
	return objs, nil
}

// bluezErr: the context's error when it ended the call, else no usable Bluetooth.
func bluezErr(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("bt: %w: %s: %w", blecore.ErrNoBluetooth, what, err)
}
