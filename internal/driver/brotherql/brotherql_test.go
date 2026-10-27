package brotherql

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

var update = flag.Bool("update", false, "rewrite golden files")

func unpackbits(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); {
		n := int(int8(src[i]))
		i++
		if n >= 0 {
			out = append(out, src[i:i+n+1]...)
			i += n + 1
		} else {
			out = append(out, bytes.Repeat(src[i:i+1], 1-n)...)
			i++
		}
	}
	return out
}

func TestPackbitsRoundTrip(t *testing.T) {
	long := bytes.Repeat([]byte{1, 2, 3}, 100)
	for _, in := range [][]byte{
		{}, {7}, {7, 7}, {1, 2}, make([]byte, 90), make([]byte, 300), long,
		{0, 0, 1, 2, 2, 2, 3, 0, 0xff, 0xff},
		append(bytes.Repeat([]byte{9}, 129), 1, 2, 3),
	} {
		got := unpackbits(packbits(in))
		if !bytes.Equal(got, in) {
			t.Errorf("roundtrip %v: got %v", in, got)
		}
	}
	if got := packbits(make([]byte, 90)); !bytes.Equal(got, []byte{257 - 90, 0}) {
		t.Errorf("blank line = % x", got)
	}
}

func TestPackLineMapping(t *testing.T) {
	spec, _ := lookupMedia("dk-22210")
	img := white(306, 1)
	img.Pix[0] = 0   // leftmost dot
	img.Pix[305] = 0 // rightmost dot
	line := packLine(img, 0, spec)
	// rightmost image column → pin 6, leftmost → pin 6+305 = 311
	if line[0] != 0x80>>6 || line[311/8] != 0x80>>(311%8) {
		t.Fatalf("line = % x", line)
	}
}

func TestParseStatus(t *testing.T) {
	base := func() []byte {
		b := make([]byte, 32)
		b[0], b[1], b[2], b[3], b[4] = 0x80, 0x20, 'B', '4', 0x39
		b[10], b[11] = 29, 0x0a
		return b
	}
	tests := []struct {
		name  string
		mod   func(b []byte)
		code  driver.Code
		media string
		done  bool
	}{
		{"ok 29mm", func(b []byte) {}, driver.OK, "dk-22210", false},
		{"die-cut 17x54", func(b []byte) { b[10], b[11], b[17] = 17, 0x0b, 54 }, driver.OK, "dk-11204", false},
		{"no media", func(b []byte) { b[8], b[10], b[11] = 0x01, 0, 0 }, driver.NoMedia, "", false},
		{"cover open", func(b []byte) { b[9], b[18] = 0x10, 0x02 }, driver.CoverOpen, "dk-22210", false},
		{"wrong media", func(b []byte) { b[9], b[18] = 0x01, 0x02 }, driver.MediaMismatch, "dk-22210", false},
		{"printed", func(b []byte) { b[18] = 0x01 }, driver.OK, "dk-22210", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.mod(b)
			s, err := parseStatus(b)
			if err != nil {
				t.Fatal(err)
			}
			if s.Code != tc.code || s.Done != tc.done || s.Model != "QL-810W" {
				t.Errorf("got %+v", s)
			}
			if (s.Media == nil && tc.media != "") || (s.Media != nil && s.Media.ID != tc.media) {
				t.Errorf("media = %+v, want %q", s.Media, tc.media)
			}
		})
	}
	if _, err := parseStatus([]byte("garbage")); err == nil {
		t.Error("garbage parsed")
	}
}

func white(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	return img
}

// testPage draws a border and a diagonal, enough to catch orientation/offset changes.
func testPage(w, h int) *image.Gray {
	img := white(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 || x == y*w/h {
				img.Pix[y*img.Stride+x] = 0
			}
		}
	}
	return img
}

func TestEncodeGolden(t *testing.T) {
	tests := []struct {
		name  string
		media string
		pages []*image.Gray
		job   driver.Job
		d     Driver
	}{
		{"29mm_one_page", "dk-22210", []*image.Gray{testPage(306, 100)}, driver.Job{}, Driver{}},
		{"29mm_two_pages_cut_last", "dk-22210", []*image.Gray{testPage(306, 50), testPage(306, 60)}, driver.Job{Cut: "last"}, Driver{}},
		{"12mm_uncompressed", "dk-22214", []*image.Gray{testPage(106, 40)}, driver.Job{}, Driver{NoCompression: true}},
		{"17x54_diecut", "dk-11204", []*image.Gray{testPage(165, 566)}, driver.Job{}, Driver{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := LookupMedia(tc.media)
			var buf bytes.Buffer
			if err := tc.d.Encode(context.Background(), &buf, tc.pages, &tc.job, &m); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", tc.name+".bin")
			if *update {
				if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("output differs from %s (run with -update after verifying on hardware)", golden)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	m, _ := LookupMedia("dk-22210")
	var d Driver
	err := d.Validate([]*image.Gray{white(696, 10)}, &driver.Job{}, &m)
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.MediaMismatch {
		t.Errorf("62mm page on 29mm tape: %v", err)
	}
	dc, _ := LookupMedia("dk-11204")
	if err := d.Validate([]*image.Gray{white(165, 600)}, &driver.Job{}, &dc); err == nil {
		t.Error("too-long die-cut label accepted")
	}
	if err := d.Validate([]*image.Gray{white(306, 10)}, &driver.Job{Cut: "sometimes"}, &m); err == nil {
		t.Error("bad cut mode accepted")
	}
}

// eofTransport is a printer connection that was closed.
type eofTransport struct{}

func (eofTransport) Open(context.Context) error  { return nil }
func (eofTransport) Write(p []byte) (int, error) { return len(p), nil }
func (eofTransport) Read([]byte) (int, error)    { return 0, io.EOF }
func (eofTransport) Close() error                { return nil }
func (eofTransport) Kind() string                { return "test" }

func TestWaitDoneStopsOnClosedConnection(t *testing.T) {
	start := time.Now()
	err := WaitDone(context.Background(), eofTransport{}, 1, 30*time.Second)
	if !errors.Is(err, driver.ErrNoReadback) || !errors.Is(err, io.EOF) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("spun for %v on a closed connection", time.Since(start))
	}
}
