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

// readReply reads one 32-byte status reply. A read timeout in the middle of a reply waits on
// (twice) for the rest, so a pause can't shift the stream by a partial status. No reply →
// driver.ErrNoReadback (wrapping the cause: EOF means the connection is gone).
func readReply(t driver.Transport) ([]byte, error) {
	buf := make([]byte, 32)
	for n, pauses := 0, 0; n < len(buf); {
		k, err := t.Read(buf[n:])
		n += k
		if err == nil {
			continue
		}
		if n > 0 && n < len(buf) && errors.Is(err, os.ErrDeadlineExceeded) && pauses < 2 {
			pauses++
			continue
		}
		if n == len(buf) {
			break
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, driver.ErrNoReadback) {
			return nil, fmt.Errorf("%w: %w", driver.ErrNoReadback, err)
		}
		return nil, err
	}
	return buf, nil
}
