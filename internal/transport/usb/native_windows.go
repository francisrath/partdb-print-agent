package usb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"golang.org/x/sys/windows"
)

// Windows binds every USB printer-class interface to its inbox usbprint.sys ("USB Printing
// Support"), vendor driver or not, so printers are found and opened without a driver swap.
var (
	usbprintGUID  = windows.GUID{Data1: 0x28d78fad, Data2: 0x5a12, Data3: 0x11d1, Data4: [8]byte{0xae, 0x5b, 0x00, 0x00, 0xf8, 0x03, 0xa8, 0xc2}}
	devpkeyParent = windows.DEVPROPKEY{FmtID: windows.DEVPROPGUID{Data1: 0x4340a6c5, Data2: 0x93fa, Data3: 0x4706, Data4: [8]byte{0x97, 0x2c, 0x7b, 0x64, 0x80, 0x08, 0xa5, 0xa7}}, PID: 8}
)

const (
	ioctlGet1284ID = 0x220034 // IOCTL_USBPRINT_GET_1284_ID (usbprint.h)
	idBufLen       = 4094     // Microsoft: larger buffers fail on some printers
	ioctlTimeout   = 2 * time.Second
	writeChunk     = 64 << 10
)

// listNative lists the printers bound to usbprint.sys. Device is the interface path to open.
func listNative() ([]Printer, error) {
	set, err := windows.SetupDiGetClassDevsEx(&usbprintGUID, "", 0, windows.DIGCF_PRESENT|windows.DIGCF_DEVICEINTERFACE, 0, "")
	if err != nil {
		return nil, err
	}
	defer set.Close()
	var out []Printer
	for i := 0; ; i++ {
		info, err := set.EnumDeviceInfo(i)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			break
		}
		if err != nil {
			return nil, err
		}
		id, err := set.DeviceInstanceID(info)
		if err != nil {
			return nil, err
		}
		paths, err := windows.CM_Get_Device_Interface_List(id, &usbprintGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
		if err != nil || len(paths) == 0 || paths[0] == "" {
			continue // unplugged while we looked
		}
		parent, _ := windows.SetupDiGetDeviceProperty(set, info, &devpkeyParent)
		parentID, _ := parent.(string)
		usbID, serial := usbIDSerial(id, parentID)
		// a printer that won't answer (busy, going away) is still listed, without its 1284 ID
		devID, _ := deviceID(paths[0])
		out = append(out, Printer{Device: paths[0], USBID: usbID, Serial: serial, IEEE1284: ParseIEEE1284(devID)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

func deviceID(path string) (string, error) {
	f, err := OpenDevice(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	d := f.(*winDevice)
	b := make([]byte, idBufLen)
	n, err := d.do(ioctlTimeout, func(n *uint32, o *windows.Overlapped) error {
		return windows.DeviceIoControl(d.h, ioctlGet1284ID, nil, 0, &b[0], uint32(len(b)), n, o)
	})
	if err != nil {
		return "", err
	}
	return parse1284Reply(b[:n]), nil
}

// winDevice is a usbprint.sys interface opened for overlapped I/O, so every read and write can
// be cancelled: a printer that answers nothing leaves a read pending forever (QL-810W), and a
// blocking read would even keep the process from exiting.
type winDevice struct {
	h        windows.Handle
	deadline time.Time
}

// OpenDevice opens a usbprint.sys interface path. usbprint doesn't enforce exclusive access, so
// the Windows spooler can still print to the same printer; we share rather than pretend otherwise.
func OpenDevice(device string) (io.ReadWriteCloser, error) {
	p, err := windows.UTF16PtrFromString(device)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", device, err)
	}
	return &winDevice{h: h}, nil
}

// do starts an overlapped operation and waits up to timeout (< 0: no limit) for it, cancelling it
// then. A cancelled operation that moved no bytes returns os.ErrDeadlineExceeded.
func (d *winDevice) do(timeout time.Duration, op func(*uint32, *windows.Overlapped) error) (int, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)
	o := &windows.Overlapped{HEvent: ev} // on the heap: the driver writes to it after op returns
	var n uint32
	err = op(&n, o)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		wait := uint32(windows.INFINITE)
		if timeout >= 0 {
			wait = uint32(timeout.Milliseconds())
		}
		if ev, werr := windows.WaitForSingleObject(o.HEvent, wait); werr != nil || ev != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(d.h, o)
		}
		err = windows.GetOverlappedResult(d.h, o, &n, true) // always: the buffers are in use until it's done
	}
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		if n == 0 {
			return 0, os.ErrDeadlineExceeded
		}
		err = nil
	}
	return int(n), err
}

// Read waits for the printer until the read deadline. A read that completes with nothing
// (PT-9500PC) returns 0, nil; Transport.Read polls on that.
func (d *winDevice) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	timeout := time.Duration(-1)
	if !d.deadline.IsZero() {
		timeout = max(time.Until(d.deadline), 0)
	}
	return d.do(timeout, func(n *uint32, o *windows.Overlapped) error { return windows.ReadFile(d.h, p, n, o) })
}

func (d *winDevice) SetReadDeadline(t time.Time) error {
	d.deadline = t
	return nil
}

// Write sends p in chunks; a chunk the printer doesn't take within drainTimeout (out of paper,
// stalled) is cancelled. A write completes when usbprint has sent it, so Close needs no drain.
func (d *winDevice) Write(p []byte) (int, error) {
	var total int
	for len(p) > 0 {
		chunk := p[:min(len(p), writeChunk)]
		n, err := d.do(drainTimeout, func(n *uint32, o *windows.Overlapped) error { return windows.WriteFile(d.h, chunk, n, o) })
		total += n
		if errors.Is(err, os.ErrDeadlineExceeded) || err == nil && n < len(chunk) {
			return total, fmt.Errorf("the printer took no data for %v (%d of %d bytes sent)", drainTimeout, total, total+len(p)-n)
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

func (d *winDevice) Close() error { return windows.CloseHandle(d.h) }
