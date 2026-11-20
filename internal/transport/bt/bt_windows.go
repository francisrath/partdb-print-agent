package bt

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() { register(dial) }

var winErrnos = errnos{
	// WSAETIMEDOUT also comes back (after ~5 s) for an address nobody answers on; WSAEACCES: not paired.
	off:       []syscall.Errno{windows.WSAETIMEDOUT, windows.WSAECONNREFUSED, windows.WSAEHOSTDOWN, windows.WSAEACCES},
	noAdapter: []syscall.Errno{windows.WSAEAFNOSUPPORT, windows.WSAENETDOWN},
	// The device answered, but SDP found no SPP record (seen after ~1 s).
	noSPP: []syscall.Errno{windows.WSAEADDRNOTAVAIL},
	// Nothing remembered; WSALookupServiceBegin reports an empty result as an error.
	noDevices: []syscall.Errno{windows.WSASERVICE_NOT_FOUND, windows.WSA_E_NO_MORE, windows.WSAENOMORE},
}

// sppUUID is the Serial Port Profile service class, 00001101-0000-1000-8000-00805F9B34FB.
var sppUUID = windows.GUID{Data1: 0x00001101, Data2: 0x0000, Data3: 0x1000,
	Data4: [8]byte{0x80, 0x00, 0x00, 0x80, 0x5F, 0x9B, 0x34, 0xFB}}

// Winsock initialises itself: package os (internal/poll) calls WSAStartup in every Go program.

func dial(ctx context.Context, address, name string, channel int) (conn, error) {
	if address == "" {
		devs, err := pairedDevices()
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
	h, err := windows.Socket(windows.AF_BTH, windows.SOCK_STREAM, windows.BTHPROTO_RFCOMM)
	if err != nil {
		return nil, mapErr(label(address, name), err, winErrnos)
	}
	s := &socket{h: h}
	sa := &windows.SockaddrBth{BtAddr: bthAddr(a), Port: uint32(channel)}
	if channel == 0 {
		sa.ServiceClassId = sppUUID // Windows looks the channel up via SDP
	}

	// A blocking connect, run under the socket's ownership (socket.do): on a timeout or ctx's end dial closes
	// the socket and returns, but the handle is only released once Connect has returned, so it can't be
	// reused under it. Close cancels the pending connect (CancelIoEx); failing that, Windows' own connect
	// timeout bounds it.
	done := make(chan error, 1)
	go func() { done <- s.do(func(h windows.Handle) error { return windows.Connect(h, sa) }) }()
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-timer.C:
		err = os.ErrDeadlineExceeded
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = s.Close()
		return nil, mapErr(label(address, name), err, winErrnos)
	}
	return s, nil
}

// pairedDevices lists the devices Windows remembers (paired or remembered), without an inquiry.
func pairedDevices() ([]Device, error) { return lookupDevices(0) }

// inquiryBlob is BTH_QUERY_DEVICE as ws2bth.h lays it out (packed: 5 bytes): ULONG LAP, reserved and zero
// (Windows runs a general inquiry only), then UCHAR length, the inquiry's length in seconds.
func inquiryBlob(secs int) [5]byte { return [5]byte{4: byte(secs)} }

// lookupDevices lists the devices Windows knows; inquiry > 0 first runs an inquiry of that many seconds
// (LUP_FLUSHCACHE), which WSALookupServiceBegin blocks for.
func lookupDevices(inquiry int) ([]Device, error) {
	const flags = windows.LUP_CONTAINERS | windows.LUP_RETURN_NAME | windows.LUP_RETURN_ADDR | windows.LUP_RETURN_TYPE
	q := windows.WSAQUERYSET{NameSpace: windows.NS_BTH}
	q.Size = uint32(unsafe.Sizeof(q))
	begin := uint32(flags)
	if inquiry > 0 {
		qd := inquiryBlob(inquiry)
		q.Blob = &windows.BLOB{Size: uint32(len(qd)), BlobData: &qd[0]}
		begin |= windows.LUP_FLUSHCACHE
	}
	var h windows.Handle
	if err := windows.WSALookupServiceBegin(&q, begin, &h); err != nil {
		return lookupErr(err, winErrnos)
	}
	defer func() { _ = windows.WSALookupServiceEnd(h) }()

	buf := make([]uint64, 512) // 4 KiB, 8-byte aligned for the pointers inside
	var devs []Device
	for {
		n := int32(len(buf) * 8)
		r := (*windows.WSAQUERYSET)(unsafe.Pointer(&buf[0]))
		err := windows.WSALookupServiceNext(h, flags, &n, r)
		switch {
		case errors.Is(err, windows.WSA_E_NO_MORE), errors.Is(err, windows.WSAENOMORE):
			return devs, nil
		case errors.Is(err, windows.WSAEFAULT) && int(n+7)/8 > len(buf):
			buf = make([]uint64, (n+7)/8)
			continue
		case err != nil:
			if _, err := lookupErr(err, winErrnos); err != nil {
				return nil, err
			}
			return devs, nil // another end-of-list code keeps what came before
		}
		if r.ServiceInstanceName == nil || r.NumberOfCsAddrs == 0 || r.SaBuffer == nil {
			continue
		}
		// LUP_RETURN_ADDR: the peer's SOCKADDR_BTH. Microsoft's samples (and Windows 11 here) put it in
		// RemoteAddr; the docs don't name the field, so LocalAddr is the fallback.
		sa := r.SaBuffer.RemoteAddr.Sockaddr
		if sa == nil {
			sa = r.SaBuffer.LocalAddr.Sockaddr
		}
		if sa == nil {
			continue
		}
		raw := (*windows.RawSockaddrBth)(unsafe.Pointer(sa))
		v := binary.LittleEndian.Uint64(raw.BtAddr[:])
		d := Device{Name: windows.UTF16PtrToString(r.ServiceInstanceName), Address: FormatAddr(fromBth(v))}
		if r.ServiceClassId != nil {
			d.Class = r.ServiceClassId.Data1 // LUP_RETURN_TYPE: the class of device
		}
		devs = append(devs, d)
	}
}

// inquiry guards the process-wide OS inquiry (a documented exception to "no global state"); it is
// held while an inquiry runs and can outlive the Scan that started it.
var inquiry sync.Mutex

// Scan runs an inquiry of about timeout (whole seconds, 1–48) and lists the devices it found and those
// Windows remembers. The inquiry can't be cancelled: on ctx's end Scan returns and it finishes alone; a
// Scan meanwhile fails at once rather than queue a second inquiry.
func Scan(ctx context.Context, timeout time.Duration) ([]Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !inquiry.TryLock() {
		return nil, errors.New("bt: a Bluetooth search is already running")
	}
	secs := min(max(int((timeout+time.Second-1)/time.Second), 1), 48)
	type result struct {
		devs []Device
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer inquiry.Unlock()
		devs, err := lookupDevices(secs)
		done <- result{devs, err}
	}()
	select {
	case r := <-done:
		return r.devs, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// socket is a blocking Winsock RFCOMM socket; deadlines become SO_RCVTIMEO / SO_SNDTIMEO.
//
// Winsock may hand a closed handle out again at once, and closesocket must not race a call that is about to
// use it, so every native call runs under do, which counts it in flight. Close marks the socket closed: with
// no call in flight it closes the handle; otherwise it cancels the pending I/O (else the call's timeout ends
// it) and the last call to return closes the handle.
type socket struct {
	h      windows.Handle
	mu     sync.Mutex
	users  int // native calls in flight
	closed bool
}

// do runs f on the handle unless the socket is closed (net.ErrClosed).
func (s *socket) do(f func(windows.Handle) error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.users++
	s.mu.Unlock()
	err := f(s.h)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.users--; s.closed && s.users == 0 {
		_ = windows.Closesocket(s.h)
	}
	return err
}

func (s *socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.users > 0 {
		// End the blocked call: shutdown alone doesn't wake a blocking WSARecv (seen on Windows 11), but
		// cancelling the handle's pending I/O does. The last call in flight closes the handle.
		_ = windows.Shutdown(s.h, windows.SHUT_RDWR)
		_ = windows.CancelIoEx(s.h, nil)
		return nil
	}
	return windows.Closesocket(s.h)
}

func timeoutMS(t time.Time) int {
	if t.IsZero() {
		return 0 // no timeout
	}
	return max(int(time.Until(t)/time.Millisecond), 1)
}

func (s *socket) SetReadDeadline(t time.Time) error {
	return s.do(func(h windows.Handle) error {
		return windows.SetsockoptInt(h, windows.SOL_SOCKET, windows.SO_RCVTIMEO, timeoutMS(t))
	})
}

func (s *socket) SetWriteDeadline(t time.Time) error {
	return s.do(func(h windows.Handle) error {
		return windows.SetsockoptInt(h, windows.SOL_SOCKET, windows.SO_SNDTIMEO, timeoutMS(t))
	})
}

func (s *socket) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	err := s.do(func(h windows.Handle) error {
		b := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
		var flags uint32
		return windows.WSARecv(h, &b, 1, &n, &flags, nil, nil)
	})
	switch {
	case errors.Is(err, windows.WSAETIMEDOUT):
		return 0, os.ErrDeadlineExceeded
	case err != nil:
		return 0, err
	case n == 0:
		return 0, io.EOF
	}
	return int(n), nil
}

func (s *socket) Write(p []byte) (int, error) {
	sent := 0
	for sent < len(p) {
		var n uint32
		err := s.do(func(h windows.Handle) error {
			b := windows.WSABuf{Len: uint32(len(p) - sent), Buf: &p[sent]}
			return windows.WSASend(h, &b, 1, &n, 0, nil, nil)
		})
		if err != nil {
			if errors.Is(err, windows.WSAETIMEDOUT) {
				err = os.ErrDeadlineExceeded
			}
			return sent, err
		}
		sent += int(n)
	}
	return sent, nil
}
