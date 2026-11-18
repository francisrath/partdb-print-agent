// Package brother drives Brother label printers over their raster protocols, clean-room from
// Brother's public command references (no brother_ql / ptouch-print code). Each model's dialect
// (from the device database) picks the protocol variant:
//   - "ql": QL-800/810W/820NWB, "Raster Command Reference" (ESC i z, 720 pins, TIFF compression)
//   - "pt-cbp": PT-9500PC, "Command Reference, CBP-RASTER Mode" (ESC i c, 384 pins, page-paced)
package brother

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() { driver.Register("brother", New) }

// New builds the driver for a Brother model.
func New(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
	switch m.Dialect {
	case "ql":
		return newQL(m, o), nil
	case "pt-cbp":
		return newPT(m), nil
	}
	return nil, &driver.Error{Code: driver.Unsupported, Detail: fmt.Sprintf("%s: unknown Brother dialect %q", m.ID, m.Dialect)}
}

// packbits is TIFF PackBits: n>=0 → n+1 literal bytes; n<0 → next byte repeated 1-n times.
func packbits(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); {
		j := i + 1
		for j < len(src) && src[j] == src[i] && j-i < 128 {
			j++
		}
		if j-i >= 2 {
			out = append(out, byte(257-(j-i)), src[i])
			i = j
			continue
		}
		j = i + 1
		for j < len(src) && j-i < 128 && !(j+1 < len(src) && src[j] == src[j+1]) {
			j++
		}
		out = append(out, byte(j-i-1))
		out = append(out, src[i:j]...)
		i = j
	}
	return out
}

// replyReader reads 32-byte status replies and keeps a partial reply across read timeouts,
// so a pause in the middle of a reply never shifts the stream.
type replyReader struct {
	t   driver.Transport
	buf [32]byte
	n   int // bytes of the current reply read so far
}

// read returns the next complete reply. A read timeout (or no read-back) returns
// driver.ErrNoReadback wrapping the cause, keeping any partial reply for the next call.
func (r *replyReader) read() ([]byte, error) {
	for r.n < len(r.buf) {
		k, err := r.t.Read(r.buf[r.n:])
		r.n += k
		if err == nil || r.n == len(r.buf) {
			continue
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, driver.ErrNoReadback) {
			return nil, fmt.Errorf("%w: %w", driver.ErrNoReadback, err) // keep the cause: EOF means the connection is gone
		}
		return nil, err
	}
	r.n = 0
	return append([]byte(nil), r.buf[:]...), nil
}

// incomplete is the error for a reply that started but never finished.
func (r *replyReader) incomplete(cause error) error {
	return &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("incomplete status reply (%d of %d bytes): %v", r.n, len(r.buf), cause)}
}

// readReply reads one reply for a one-off status request: a reply that stops partway gets
// two more read timeouts to finish, then it's a framing error (never retried on a shifted stream).
func readReply(t driver.Transport) ([]byte, error) {
	r := &replyReader{t: t}
	for pauses := 0; ; pauses++ {
		b, err := r.read()
		if err == nil || r.n == 0 {
			return b, err
		}
		if !errors.Is(err, driver.ErrNoReadback) || errors.Is(err, io.EOF) || pauses == 2 {
			return nil, r.incomplete(err)
		}
	}
}
