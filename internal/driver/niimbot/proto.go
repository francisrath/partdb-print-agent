package niimbot

import (
	"encoding/binary"
	"image"
	"math/bits"
)

// The Niimbot packet protocol, from niimbluelib (MIT) and the printers.niim.blue protocol notes (clean room).
// A packet is `55 55 cmd len data… xor AA AA`, xor over cmd, len and data; u16 fields are big-endian.
// Every request has its own answer command (see answers); 0xDB (error code) or 0x00 (not supported) may come
// back instead of any answer.
const (
	cmdPrintStart   = 0x01
	cmdPageStart    = 0x03 // → 0x04
	cmdPageSize     = 0x13 // → 0x14
	cmdQuantity     = 0x15 // → 0x16
	cmdPrintClear   = 0x20 // → 0x30
	cmdDensity      = 0x21 // → 0x31
	cmdLabelType    = 0x23 // → 0x33
	cmdRFID         = 0x1A // → 0x1B
	cmdEmptyRows    = 0x84 // row u16, repeat: blank rows, no answer
	cmdBitmapRow    = 0x85 // row u16, 3 count bytes, repeat, bits: no answer
	cmdCheckLine    = 0x86 // row u16, 01 → 0xD3 (B21)
	cmdPrintStatus  = 0xA3 // → 0xB3: page u16, progress, feed progress[, …, error]
	cmdConnect      = 0xC1 // → 0xC2: protocol level; sent with a 03 in front
	cmdHeartbeat    = 0xDC // type → 0xDD/0xDE/0xDF/0xD9
	cmdPageEnd      = 0xE3 // → 0xE4
	cmdPrintEnd     = 0xF3 // → 0xF4: 01 = done, 00 = still printing
	rspError        = 0xDB
	rspNotSupported = 0x00
	rspPageIndex    = 0xE0 // unsolicited page count (D11)
	rspCheckLine    = 0xD3
	rspHeartbeatV4  = 0xD9
	maxRepeat       = 255 // the repeat field is one byte
	checkLineEvery  = 200 // rows between CheckLine packets (B21)
)

// packet frames cmd and data.
func packet(cmd byte, data ...byte) []byte {
	p := make([]byte, 0, len(data)+7)
	p = append(p, 0x55, 0x55, cmd, byte(len(data)))
	p = append(p, data...)
	return append(p, checksum(p[2:]), 0xAA, 0xAA)
}

func checksum(b []byte) byte {
	var x byte
	for _, c := range b {
		x ^= c
	}
	return x
}

func u16(v int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }

// connectPacket is the only packet with a byte (03) before its header.
var connectPacket = append([]byte{0x03}, packet(cmdConnect, 0x01)...)

type reply struct {
	cmd  byte
	data []byte
}

// nextPacket takes the first whole packet off a notification stream, skipping junk before a 55 55 header
// and packets whose checksum or tail is wrong. ok is false until a whole packet is there; rest keeps the tail.
func nextPacket(b []byte) (r reply, rest []byte, ok bool) {
	for {
		for len(b) > 0 && b[0] != 0x55 {
			b = b[1:]
		}
		if len(b) < 4 {
			return r, b, false
		}
		if b[1] != 0x55 || b[2] == 0x55 { // no command is 0x55: that is a header after a stray 55
			b = b[1:]
			continue
		}
		n := int(b[3])
		if len(b) < 7+n {
			return r, b, false
		}
		if checksum(b[2:4+n]) != b[4+n] || b[5+n] != 0xAA || b[6+n] != 0xAA {
			b = b[1:]
			continue
		}
		return reply{cmd: b[2], data: b[4 : 4+n]}, b[7+n:], true
	}
}

// rowPackets encodes a page as rows across the head: dots per row (whole bytes, MSB = first dot, 1 = black),
// runs of identical rows sent once with a repeat count, blank runs as empty-row packets. Total mode puts
// the black dot count in the last two count bytes (low byte first); otherwise the counts are per third of
// a head of head dots, falling back to total mode for rows wider than that. With checkLine, a run ends at
// every checkLineEvery rows and a CheckLine request follows (B21).
func rowPackets(img *image.Gray, dots, head int, total, checkLine bool) [][]byte {
	r := img.Bounds()
	rowBytes := (dots + 7) / 8
	third := head / 8 / 3
	total = total || third == 0 || rowBytes > 3*third
	rows := make([][]byte, r.Dy())
	for y := range rows {
		row := make([]byte, rowBytes)
		for x := range min(dots, r.Dx()) {
			if img.GrayAt(r.Min.X+x, r.Min.Y+y).Y < 128 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		rows[y] = row
	}
	var out [][]byte
	for y := 0; y < len(rows); {
		n := 1
		for y+n < len(rows) && n < maxRepeat && string(rows[y+n]) == string(rows[y]) &&
			(!checkLine || (y+n)%checkLineEvery != 0) {
			n++
		}
		var black [3]int
		for i, b := range rows[y] {
			black[min(i/max(third, 1), 2)] += bits.OnesCount8(b)
		}
		sum := black[0] + black[1] + black[2]
		switch {
		case sum == 0:
			out = append(out, packet(cmdEmptyRows, append(u16(y), byte(n))...))
		case total:
			d := append(u16(y), 0, byte(sum), byte(sum>>8), byte(n))
			out = append(out, packet(cmdBitmapRow, append(d, rows[y]...)...))
		default:
			d := append(u16(y), byte(black[0]), byte(black[1]), byte(black[2]), byte(n))
			out = append(out, packet(cmdBitmapRow, append(d, rows[y]...)...))
		}
		y += n
		if checkLine && y%checkLineEvery == 0 {
			out = append(out, packet(cmdCheckLine, append(u16(y-1), 0x01)...))
		}
	}
	return out
}
