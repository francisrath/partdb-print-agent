package serial

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// DCB flag bits (winbase.h); x/sys/windows has the struct but not these.
const (
	dcbBinary           = 1 << 0
	dcbParity           = 1 << 1
	dcbOutxCtsFlow      = 1 << 2
	dcbDtrControlEnable = 1 << 4
	dcbOutX             = 1 << 8
	dcbRtsControlEnable = 1 << 12
	dcbRtsHandshake     = 2 << 12
)

const (
	noParity, oddParity, evenParity = 0, 1, 2
	oneStopBit, twoStopBits         = 0, 2
)

type winPort struct {
	h    windows.Handle
	baud int
	last time.Duration // read timeout currently set
}

func openPort(device string, l Line) (port, error) {
	name := device
	if !strings.HasPrefix(name, `\\.\`) {
		name = `\\.\` + name // COM10 and up need the device namespace
	}
	p16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	p := &winPort{h: h, baud: l.Baud, last: -1}
	if err := p.setup(l); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return p, nil
}

func (p *winPort) setup(l Line) error {
	var d windows.DCB
	d.DCBlength = uint32(unsafe.Sizeof(d))
	if err := windows.GetCommState(p.h, &d); err != nil {
		return fmt.Errorf("not a serial port? %w", err)
	}
	d.BaudRate = uint32(l.Baud)
	d.ByteSize = 8 // config allows 8 data bits only (binary raster data)
	d.Flags = dcbBinary | dcbDtrControlEnable | dcbRtsControlEnable
	switch l.Framing[1] {
	case 'E':
		d.Parity, d.Flags = evenParity, d.Flags|dcbParity
	case 'O':
		d.Parity, d.Flags = oddParity, d.Flags|dcbParity
	default:
		d.Parity = noParity
	}
	d.StopBits = oneStopBit
	if l.Framing[2] == '2' {
		d.StopBits = twoStopBits
	}
	switch l.Flow {
	case "xonxoff": // the printer's XOFF stops our output
		d.Flags |= dcbOutX
		d.XonChar, d.XoffChar = 0x11, 0x13
	case "rtscts":
		d.Flags = d.Flags&^dcbRtsControlEnable | dcbOutxCtsFlow | dcbRtsHandshake
	}
	if err := windows.SetCommState(p.h, &d); err != nil {
		return err
	}
	return p.setReadTimeout(2 * time.Second)
}

// setReadTimeout makes ReadFile return as soon as a byte is there, or empty after d. Writes may take
// twice the line's time per byte plus 30 s (flow control holding them), then fail.
func (p *winPort) setReadTimeout(d time.Duration) error {
	if d == p.last {
		return nil
	}
	ms := uint32(max(d.Milliseconds(), 1))
	perByte := uint32(max(20000/p.baud, 1)) // 10 bits per byte, twice over
	err := windows.SetCommTimeouts(p.h, &windows.CommTimeouts{
		ReadIntervalTimeout: 0xFFFFFFFF, ReadTotalTimeoutMultiplier: 0xFFFFFFFF, ReadTotalTimeoutConstant: ms,
		WriteTotalTimeoutMultiplier: perByte, WriteTotalTimeoutConstant: uint32(stallTimeout.Milliseconds()),
	})
	if err == nil {
		p.last = d
	}
	return err
}

const stallTimeout = 30 * time.Second

func (p *winPort) read(b []byte, timeout time.Duration) (int, error) {
	if err := p.setReadTimeout(timeout); err != nil {
		return 0, err
	}
	var n uint32
	err := windows.ReadFile(p.h, b, &n, nil)
	return int(n), err
}

func (p *winPort) write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.h, b, &n, nil)
	if err == nil && int(n) < len(b) {
		err = fmt.Errorf("the printer took only %d of %d bytes (flow control holding, or wrong flow setting)", n, len(b))
	}
	return int(n), err
}

// drain waits until the driver's output queue is empty. Not FlushFileBuffers: write timeouts don't bound
// it, so a printer holding XOFF or CTS would keep Close (and the port) forever.
func (p *winPort) drain(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var errs uint32
		var st windows.ComStat
		if err := windows.ClearCommError(p.h, &errs, &st); err != nil {
			return err
		}
		if st.CBOutQue == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%d bytes still queued after %v: the end of the job may be lost", st.CBOutQue, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *winPort) close() error { return windows.CloseHandle(p.h) }

// Ports lists the COM ports Windows knows (HKLM\HARDWARE\DEVICEMAP\SERIALCOMM), sorted.
func Ports() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if v, _, err := k.GetStringValue(n); err == nil {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// Present: Windows lists the COM port (its adapter is plugged in).
func Present(device string) bool {
	return slices.ContainsFunc(Ports(), func(p string) bool { return strings.EqualFold(p, strings.TrimPrefix(device, `\\.\`)) })
}
