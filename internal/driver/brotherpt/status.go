package brotherpt

import (
	"fmt"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// Status types (byte 18, p.16).
const (
	statusReply     = 0x00
	statusCompleted = 0x01
	statusError     = 0x02
	statusPhase     = 0x06
)

// mediaTypes maps byte 11 (p.16).
var mediaTypes = map[byte]string{0x01: "laminated", 0x02: "lettering", 0x03: "non-laminated", 0x08: "av", 0x09: "hg"}

// parseStatus decodes the 32-byte status (p.14–17). The PT-9500PC has no model byte.
func parseStatus(b []byte) (*driver.Status, byte, error) {
	if len(b) != 32 || b[0] != 0x80 || b[1] != 0x20 || b[2] != 'B' {
		return nil, 0, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("bad status reply % x", b)}
	}
	s := &driver.Status{Code: driver.OK, Model: model, MediaType: mediaTypes[b[11]]}
	e1, e2 := b[8], b[9]
	for _, e := range []struct {
		hit    bool
		code   driver.Code
		detail string
	}{
		{e1&0x01 != 0, driver.NoMedia, "no media"},
		{e1&0x02 != 0, driver.NoMedia, "end of media"},
		{e1&0x04 != 0, driver.CutterJam, "cutter jam"},
		{e2&0x01 != 0, driver.MediaMismatch, "replace media"},
		{e2&0x10 != 0, driver.CoverOpen, "cover open"},
		{e2&0x0e != 0, driver.CommsError, fmt.Sprintf("communication/buffer error %02x", e2)},
		{e1&0xf8 != 0 || e2&0xe0 != 0, driver.Unknown, fmt.Sprintf("error bits %02x %02x", e1, e2)},
	} {
		if e.hit {
			s.Code, s.Detail = e.code, e.detail
			break
		}
	}
	if w := int(b[10]); w != 0 {
		if m, ok := MediaForWidth(w); ok {
			s.Media = &m
		} else {
			s.Media = &driver.Media{ID: "unknown", Kind: driver.Continuous, WidthMM: float64(w)}
		}
	}
	typ := b[18]
	s.Done = typ == statusCompleted
	if typ == statusError && s.Code == driver.OK {
		s.Code = driver.Unknown
	}
	return s, typ, nil
}
