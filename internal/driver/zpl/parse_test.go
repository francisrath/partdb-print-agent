package zpl

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/francisrath/pdb-print/internal/driver"
)

// Replies as the friend's GK420d sent them (zebra-probe log, 2026-11-18).
const (
	hsIdle = "\x02030,0,0,0812,000,0,0,0,000,0,0,0\x03\r\n\x02000,0,0,0,0,2,6,0,00000000,1,000\x03\r\n\x021234,0\x03\r\n"
	hsBusy = "\x02030,0,0,0812,001,0,0,0,000,0,0,0\x03\r\n\x02000,0,0,0,0,2,6,0,00000001,1,000\x03\r\n\x021234,0\x03\r\n"
	hqesOK = "\x02\r\n\r\n  PRINTER STATUS                            \r\n   ERRORS:         0 00000000 00000000      \r\n   WARNINGS:       0 00000000 00000000      \r\n\x03\r\n"
	hiGK   = "\x02GK420d-200dpi,V61.17.11Z,8,2104KB\x03\r\n"
	hhPart = "\x02  15.0                DARKNESS          \r\n  799                 PRINT WIDTH       \r\n  0812                LABEL LENGTH      \r\n\x03\r\n"
)

func TestCRC16XMODEM(t *testing.T) {
	if got := crc16XMODEM([]byte("123456789")); got != 0x31C3 { // the standard check value
		t.Errorf("crc = %04X", got)
	}
}

func TestParseHS(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		want  hs
	}{
		"idle": {hsIdle, hs{LabelLengthDots: 812}},
		"busy": {hsBusy, hs{LabelLengthDots: 812, Formats: 1, LabelsRemaining: 1}},
		"paper out, paused, head up": {strings.Replace(strings.Replace(hsIdle, "030,0,0,", "030,1,1,", 1), "000,0,0,0,0,2", "000,0,1,0,0,2", 1),
			hs{PaperOut: true, Paused: true, HeadUp: true, LabelLengthDots: 812}},
	} {
		got, err := parseHS([]byte(tc.reply))
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v %v, want %+v", name, got, err, tc.want)
		}
	}
	if _, err := parseHS([]byte("\x02garbage\x03")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParseHQES(t *testing.T) {
	q, err := parseHQES([]byte(hqesOK))
	if err != nil || q.Errors != 0 || q.Warnings != 0 {
		t.Fatalf("%+v %v", q, err)
	}
	if c, _ := q.code(); c != driver.OK {
		t.Errorf("ok reply: %s", c)
	}
	for bits, want := range map[string]driver.Code{
		"00000001": driver.NoMedia, "00000002": driver.RibbonOut, "00000004": driver.CoverOpen,
		"00000008": driver.CutterJam, "00000010": driver.Overheat, "00010000": driver.Paused,
	} {
		q, err := parseHQES([]byte(strings.Replace(hqesOK, "ERRORS:         0 00000000 00000000", "ERRORS:         1 00000000 "+bits, 1)))
		if c, detail := q.code(); err != nil || c != want || detail == "" {
			t.Errorf("errors %s: %s %q %v, want %s", bits, c, detail, err, want)
		}
	}
}

func TestParseHI(t *testing.T) {
	model, dpm, err := parseHI([]byte(hiGK))
	if err != nil || model != "GK420d-200dpi" || dpm != 8 {
		t.Errorf("%q %d %v", model, dpm, err)
	}
}

func TestParseHH(t *testing.T) {
	if w, ok := parseHHPrintWidth([]byte(hhPart)); !ok || w != 799 {
		t.Errorf("%d %v", w, ok)
	}
}

// formats splits an encoded stream into its ^XA…^XZ formats.
func formats(b []byte) []string {
	return regexp.MustCompile(`(?s)\^XA.*?\^XZ`).FindAllString(string(b), -1)
}

// decode reverses ^GFA …:Z64:… into rows of bits, checking sizes and the CRC.
func decode(t *testing.T, f string) (pw, ll int, rows []string) {
	t.Helper()
	m := regexp.MustCompile(`\^PW(\d+)\^LL(\d+)\^FO0,0\^GFA,(\d+),(\d+),(\d+),:Z64:([A-Za-z0-9+/=]+):([0-9A-F]{4})\^FS`).FindStringSubmatch(f)
	if m == nil {
		t.Fatalf("no ^GFA Z64 in %q", f)
	}
	pw, _ = strconv.Atoi(m[1])
	ll, _ = strconv.Atoi(m[2])
	total, _ := strconv.Atoi(m[3])
	rowBytes, _ := strconv.Atoi(m[5])
	if m[3] != m[4] {
		t.Errorf("^GFA byte counts differ: %s %s", m[3], m[4])
	}
	if got := fmt.Sprintf("%04X", crc16XMODEM([]byte(m[6]))); got != m[7] {
		t.Errorf("crc %s, sent %s", got, m[7])
	}
	z, _ := base64.StdEncoding.DecodeString(m[6])
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if len(raw) != total || total%rowBytes != 0 {
		t.Fatalf("raster %d bytes, header says %d (%d per row)", len(raw), total, rowBytes)
	}
	for i := 0; i < len(raw); i += rowBytes {
		var s strings.Builder
		for _, b := range raw[i : i+rowBytes] {
			fmt.Fprintf(&s, "%08b", b)
		}
		rows = append(rows, s.String())
	}
	return pw, ll, rows
}

// gray makes an image from rows of '#' (black) and '.' (white).
func gray(rows ...string) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, len(rows[0]), len(rows)))
	for y, r := range rows {
		for x, c := range r {
			if c == '.' {
				img.Pix[y*img.Stride+x] = 255
			}
		}
	}
	return img
}

func TestEncodePage(t *testing.T) {
	var b bytes.Buffer
	if err := encodePage(&b, gray("#.#.#.#.#.", ".#.#.#.#.#"), 10, 2); err != nil {
		t.Fatal(err)
	}
	f := formats(b.Bytes())
	if len(f) != 1 || !strings.HasPrefix(f[0], "^XA^PON^PMN^LH0,0^LS0^LT0^PW10^LL2^FO0,0^GFA,") {
		t.Fatalf("format %q", b.String())
	}
	_, _, rows := decode(t, f[0])
	if want := []string{"1010101010000000", "0101010101000000"}; strings.Join(rows, " ") != strings.Join(want, " ") {
		t.Errorf("rows %v, want %v (black = 1, MSB first, padded to whole bytes)", rows, want)
	}
}

// A page wider than the print width is clipped on the right; a narrower one is padded white.
func TestEncodeClipsAndPads(t *testing.T) {
	var b bytes.Buffer
	_ = encodePage(&b, gray("##########"), 4, 1)
	if _, _, rows := decode(t, formats(b.Bytes())[0]); rows[0] != "11110000" {
		t.Errorf("clipped row %s", rows[0])
	}
	b.Reset()
	_ = encodePage(&b, gray("##"), 12, 2)
	if pw, ll, rows := decode(t, formats(b.Bytes())[0]); pw != 12 || ll != 2 || rows[0] != "1100000000000000" || rows[1] != "0000000000000000" {
		t.Errorf("padded %d %d %v", pw, ll, rows)
	}
}
