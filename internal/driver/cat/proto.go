package cat

import (
	"image"
	"io"
)

// The "51 78" protocol of BLE cat printers, from public write-ups and our MX10 spike (clean room).
const (
	cmdFeed     = 0xA1 // paper feed, LE16 lines
	cmdRow      = 0xA2 // one raster row, 48 bytes, LSB = leftmost dot, 1 = black
	cmdStatus   = 0xA3 // status request (00) and its reply (flags, 01, tt)
	cmdQuality  = 0xA4
	cmdLattice  = 0xA6
	cmdEnergy   = 0xAF // heat, LE16
	cmdSpeed    = 0xBD // speed divider: higher = slower = darker
	cmdMode     = 0xBE // 00 = image
	cmdRowRLE   = 0xBF // one raster row as runs colour<<7|count
	cmdDone     = 0xD3 // unsolicited after a job (payload 19): taken as "printed something"
	headDots    = 384  // when the model doesn't say
	maxFrame    = 512  // notifications are tiny; a longer length is junk
	maxEnergy   = 0x8000
	speedFinish = 0x19 // speed sent after the image
)

var (
	latticeStart = []byte{0xAA, 0x55, 0x17, 0x38, 0x44, 0x5F, 0x5F, 0x5F, 0x44, 0x38, 0x2C}
	latticeEnd   = []byte{0xAA, 0x55, 0x17, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x17}
)

// crc8 is CRC-8 with polynomial 0x07, init 0 (CRC-8/SMBUS), over a frame's payload.
func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// frame is `51 78 cmd 00 len16le payload crc8 FF`.
func frame(cmd byte, payload ...byte) []byte {
	f := make([]byte, 0, len(payload)+8)
	f = append(f, 0x51, 0x78, cmd, 0x00, byte(len(payload)), byte(len(payload)>>8))
	f = append(f, payload...)
	return append(f, crc8(payload), 0xFF)
}

// rleLine encodes a packed row as runs of one colour (colour<<7 | count, count 1–127), dots in
// print order. ok is false unless the runs are shorter than the raw row.
func rleLine(row []byte) ([]byte, bool) {
	var out []byte
	colour, n := byte(0), byte(0)
	for i := range len(row) * 8 {
		c := row[i/8] >> (i % 8) & 1
		if n > 0 && (c != colour || n == 127) {
			out = append(out, colour<<7|n)
			n = 0
		}
		colour = c
		n++
		if len(out) >= len(row) { // a run is still pending, so it can't get shorter
			return nil, false
		}
	}
	if n > 0 {
		out = append(out, colour<<7|n)
	}
	return out, len(out) < len(row)
}

// encodePage writes one page: setup, the rows top first (runs where shorter), finish and feed.
// Dots past the head's width (dots; 0 = 384) are cut off; a narrower page is padded white.
func encodePage(w io.Writer, img *image.Gray, dots int, energy uint16, speed byte, feed uint16) error {
	energy = min(energy, maxEnergy)
	var b []byte
	b = append(b, frame(cmdQuality, 0x33)...)
	b = append(b, frame(cmdEnergy, byte(energy), byte(energy>>8))...)
	b = append(b, frame(cmdSpeed, speed)...)
	b = append(b, frame(cmdMode, 0x00)...)
	b = append(b, frame(cmdLattice, latticeStart...)...)
	r := img.Bounds()
	if dots <= 0 {
		dots = headDots
	}
	row := make([]byte, (dots+7)/8)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		clear(row)
		for x := 0; x < min(r.Dx(), dots); x++ {
			if img.GrayAt(r.Min.X+x, y).Y < 128 {
				row[x/8] |= 1 << (x % 8)
			}
		}
		if runs, ok := rleLine(row); ok {
			b = append(b, frame(cmdRowRLE, runs...)...)
		} else {
			b = append(b, frame(cmdRow, row...)...)
		}
	}
	b = append(b, frame(cmdLattice, latticeEnd...)...)
	b = append(b, frame(cmdSpeed, speedFinish)...)
	b = append(b, frame(cmdFeed, byte(feed), byte(feed>>8))...)
	_, err := w.Write(b)
	return err
}

type catFrame struct {
	cmd     byte
	payload []byte
}

// nextFrame takes the first whole frame off a notification stream, skipping bytes before it and
// frames with a bad checksum or an impossible length. ok is false until a whole frame is there; rest keeps the tail.
func nextFrame(b []byte) (f catFrame, rest []byte, ok bool) {
	for {
		for len(b) > 0 && b[0] != 0x51 {
			b = b[1:]
		}
		if len(b) < 2 || (b[1] == 0x78 && len(b) < 6) {
			return f, b, false
		}
		if b[1] != 0x78 {
			b = b[1:]
			continue
		}
		n := int(b[4]) | int(b[5])<<8
		if n > maxFrame {
			b = b[1:]
			continue
		}
		if len(b) < 6+n+2 {
			return f, b, false
		}
		payload := b[6 : 6+n]
		if crc8(payload) != b[6+n] || b[7+n] != 0xFF {
			b = b[1:]
			continue
		}
		return catFrame{cmd: b[2], payload: payload}, b[8+n:], true
	}
}
