package cat

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func TestCRC8(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want byte
	}{
		{[]byte{0x33}, 0x99},
		{[]byte{0x70, 0x00}, 0xA2},
		{nil, 0x00},
	} {
		if got := crc8(tc.in); got != tc.want {
			t.Errorf("crc8(% x) = %#02x, want %#02x", tc.in, got, tc.want)
		}
	}
}

func TestFrame(t *testing.T) {
	for _, tc := range []struct {
		cmd     byte
		payload []byte
		want    []byte
	}{
		{0xA4, []byte{0x33}, []byte{0x51, 0x78, 0xA4, 0x00, 0x01, 0x00, 0x33, 0x99, 0xFF}},
		{0xA1, []byte{0x70, 0x00}, []byte{0x51, 0x78, 0xA1, 0x00, 0x02, 0x00, 0x70, 0x00, 0xA2, 0xFF}},
		{0xA3, []byte{0x00}, []byte{0x51, 0x78, 0xA3, 0x00, 0x01, 0x00, 0x00, 0x00, 0xFF}},
	} {
		if got := frame(tc.cmd, tc.payload...); !bytes.Equal(got, tc.want) {
			t.Errorf("frame(%#02x, % x) = % x, want % x", tc.cmd, tc.payload, got, tc.want)
		}
	}
	// 300-byte payload: the length is 16-bit little-endian
	if f := frame(0xA2, make([]byte, 300)...); f[4] != 0x2C || f[5] != 0x01 || len(f) != 308 {
		t.Errorf("long frame header % x, len %d", f[:6], len(f))
	}
}

func TestNextFrame(t *testing.T) {
	stream := append([]byte{0x00, 0x51}, frame(0xAE, 0x10)...) // junk, then flow control
	stream = append(stream, frame(0xA3, 0x80, 0x01, 0x95)...)
	stream = append(stream, frame(0xA3, 0x00)[:5]...) // partial
	var cmds []byte
	for {
		f, rest, ok := nextFrame(stream)
		if !ok {
			break
		}
		cmds, stream = append(cmds, f.cmd), rest
	}
	if !bytes.Equal(cmds, []byte{0xAE, 0xA3}) || len(stream) != 5 {
		t.Errorf("frames %x, %d bytes left", cmds, len(stream))
	}
}

func TestNextFrameBogusLength(t *testing.T) {
	stream := append([]byte{0x51, 0x78, 0xA3, 0x00, 0xFF, 0xFF}, frame(0xA3, 0x80, 0x01, 0x95)...)
	f, _, ok := nextFrame(stream)
	if !ok || f.cmd != 0xA3 || f.payload[0] != 0x80 {
		t.Errorf("after a bogus header: %+v, %v", f, ok)
	}
}

func TestEncodeHeadWidth(t *testing.T) {
	var b bytes.Buffer
	if err := encodePage(&b, page(384, "#"), 96, 0x6000, 0x80, 0); err != nil {
		t.Fatal(err)
	}
	if want := frame(0xBF, 0x81, 0x5F); !bytes.Contains(b.Bytes(), want) { // 1 black + 95 white: a 12-byte row
		t.Errorf("96-dot head: % x", b.Bytes())
	}
}

func TestRLE(t *testing.T) {
	half := make([]byte, 48) // left half black
	for i := range 24 {
		half[i] = 0xFF
	}
	noisy := bytes.Repeat([]byte{0x55}, 48)
	for _, tc := range []struct {
		name string
		row  []byte
		want []byte
		ok   bool
	}{
		{"white", make([]byte, 48), []byte{0x7F, 0x7F, 0x7F, 0x03}, true},
		{"black", bytes.Repeat([]byte{0xFF}, 48), []byte{0xFF, 0xFF, 0xFF, 0x83}, true},
		{"mixed", half, []byte{0xFF, 0xC1, 0x7F, 0x41}, true},
		{"one dot at the left", append([]byte{0x01}, make([]byte, 47)...), []byte{0x81, 0x7F, 0x7F, 0x7F, 0x02}, true},
		{"noisy", noisy, nil, false},
		{"runs exactly as long as the row", bytes.Repeat([]byte{0xFF, 0x00}, 24), nil, false},
	} {
		got, ok := rleLine(tc.row)
		if ok != tc.ok || (ok && !bytes.Equal(got, tc.want)) {
			t.Errorf("%s: rleLine = % x, %v; want % x, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// page builds a w-wide image from rows of '#' (black) and '.'.
func page(w int, rows ...string) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, len(rows)))
	for y, r := range rows {
		for x := range w {
			c := color.Gray{Y: 255}
			if x < len(r) && r[x] == '#' {
				c.Y = 0
			}
			img.SetGray(x, y, c)
		}
	}
	return img
}

func TestEncodeGolden(t *testing.T) {
	// row 0: left half black (RLE); row 1: every other dot from the left (raw A2, 0x55 bytes)
	var alt []byte
	for range 192 {
		alt = append(alt, '#', '.')
	}
	img := page(384, string(bytes.Repeat([]byte{'#'}, 192)), string(alt))
	var b bytes.Buffer
	if err := encodePage(&b, img, 384, 0x6000, 0x80, 80); err != nil {
		t.Fatal(err)
	}
	h := func(cmd byte, n int) []byte { return []byte{0x51, 0x78, cmd, 0x00, byte(n), 0x00} }
	var want []byte
	add := func(p ...[]byte) {
		for _, x := range p {
			want = append(want, x...)
		}
	}
	add(h(0xA4, 1), []byte{0x33, 0x99, 0xFF})
	add(h(0xAF, 2), []byte{0x00, 0x60, crc8([]byte{0x00, 0x60}), 0xFF})
	add(h(0xBD, 1), []byte{0x80, crc8([]byte{0x80}), 0xFF})
	add(h(0xBE, 1), []byte{0x00, 0x00, 0xFF})
	start := []byte{0xAA, 0x55, 0x17, 0x38, 0x44, 0x5F, 0x5F, 0x5F, 0x44, 0x38, 0x2C}
	add(h(0xA6, 11), start, []byte{crc8(start), 0xFF})
	add(h(0xBF, 4), []byte{0xFF, 0xC1, 0x7F, 0x41, crc8([]byte{0xFF, 0xC1, 0x7F, 0x41}), 0xFF})
	raw := bytes.Repeat([]byte{0x55}, 48)
	add(h(0xA2, 48), raw, []byte{crc8(raw), 0xFF})
	end := []byte{0xAA, 0x55, 0x17, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x17}
	add(h(0xA6, 11), end, []byte{crc8(end), 0xFF})
	add(h(0xBD, 1), []byte{0x19, crc8([]byte{0x19}), 0xFF})
	add(h(0xA1, 2), []byte{0x50, 0x00, crc8([]byte{0x50, 0x00}), 0xFF})
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("encodePage:\n got % x\nwant % x", b.Bytes(), want)
	}
}

func TestEncodeEnergyCapped(t *testing.T) {
	var b bytes.Buffer
	if err := encodePage(&b, page(384, ""), 384, 0xC000, 0x80, 0); err != nil {
		t.Fatal(err)
	}
	if want := frame(0xAF, 0x00, 0x80); !bytes.Contains(b.Bytes(), want) {
		t.Errorf("energy not capped at 0x8000: % x", b.Bytes())
	}
}

func TestEncodeNarrowAndWidePages(t *testing.T) {
	for _, w := range []int{100, 384, 500} {
		var b bytes.Buffer
		if err := encodePage(&b, page(w, "#"), 384, 0x6000, 0x80, 0); err != nil {
			t.Fatalf("width %d: %v", w, err)
		}
		// one black dot at the left, the rest white
		if want := frame(0xBF, 0x81, 0x7F, 0x7F, 0x7F, 0x02); !bytes.Contains(b.Bytes(), want) {
			t.Errorf("width %d: row % x", w, b.Bytes())
		}
	}
}
