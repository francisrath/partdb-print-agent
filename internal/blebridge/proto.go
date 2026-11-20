// Package blebridge is the stdin/stdout protocol between pdb-print and its macOS Bluetooth helper
// pdb-print-ble. A frame is type(1) len(4, big-endian) payload. The client sends Open (JSON OpenRequest),
// then Write frames, then Close; the helper answers Open and each Write with OK or Error, and sends Notify
// frames (every notification from the printer, flow control included) at any time. When the link drops
// the helper exits; the client sees end of file.
package blebridge

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame types.
const (
	TypeOpen   byte = 'O' // client → helper: JSON OpenRequest
	TypeWrite  byte = 'W' // client → helper: bytes for the printer
	TypeClose  byte = 'C' // client → helper: disconnect and exit
	TypeOK     byte = 'K' // helper → client: Open done (JSON OK) or Write done (empty)
	TypeNotify byte = 'N' // helper → client: notification bytes
	TypeError  byte = 'E' // helper → client: Open or Write failed (error text)
)

// MaxFrame is the largest payload either side sends or accepts.
const MaxFrame = 1 << 20

// OpenRequest selects the printer (the address wins, as in config.Transport) and its link: BLE with its
// GATT layout, or (Kind "rfcomm") Bluetooth Classic serial port on Channel (0 = look up the SPP record).
// For rfcomm, Write frames go to the channel, incoming data comes back as Notify frames and OK.MTU is the
// channel's MTU.
type OpenRequest struct {
	Kind    string `json:"kind,omitempty"` // "" = BLE, "rfcomm"
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
	Channel int    `json:"channel,omitempty"` // rfcomm only
	Service string `json:"service,omitempty"` // BLE only: UUIDs as 128-bit text
	Write   string `json:"write,omitempty"`
	Notify  string `json:"notify,omitempty"`
	GapMS   int    `json:"gap_ms"` // BLE only: pause between chunks
}

// KindRFCOMM is OpenRequest.Kind for a Bluetooth Classic serial port link.
const KindRFCOMM = "rfcomm"

// OK is the payload of the OK answer to Open.
type OK struct {
	MTU int `json:"mtu"` // bytes per chunk
}

// Write sends one frame in a single w.Write.
func Write(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("blebridge: %d-byte frame exceeds %d", len(payload), MaxFrame)
	}
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(len(payload)))
	copy(b[5:], payload)
	_, err := w.Write(b)
	return err
}

// Read reads one frame. It returns io.EOF only at a frame boundary; a cut-off frame is io.ErrUnexpectedEOF.
func Read(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("blebridge: %d-byte frame exceeds %d", n, MaxFrame)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return h[0], p, nil
}

// ClassicDevice is one line of the helper's scan-classic output: a Bluetooth Classic device macOS
// knows (paired) or found in an inquiry.
type ClassicDevice struct {
	Address string `json:"address"` // AA:BB:CC:DD:EE:FF
	Name    string `json:"name"`
	Class   uint32 `json:"class"` // class of device
}

// MergeClassic joins device lists (paired first, then an inquiry's) into one entry per address, in first-seen
// order. A later list only fills in a name or class the earlier entry lacks: a paired device not yet seen in
// an inquiry may have neither, and the name pairing knows is the one pdb-print dials by.
func MergeClassic(lists ...[]ClassicDevice) []ClassicDevice {
	idx := map[string]int{}
	var all []ClassicDevice
	for _, l := range lists {
		for _, d := range l {
			i, ok := idx[d.Address]
			if !ok {
				idx[d.Address] = len(all)
				all = append(all, d)
				continue
			}
			if all[i].Name == "" {
				all[i].Name = d.Name
			}
			if all[i].Class == 0 {
				all[i].Class = d.Class
			}
		}
	}
	return all
}
