package brotherpt

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

var update = flag.Bool("update", false, "rewrite golden files")

func white(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	return img
}

// testPage draws a border, a diagonal and a blank band, enough to catch orientation,
// offset and zero-line changes.
func testPage(w, h int) *image.Gray {
	img := white(w, h)
	for y := 0; y < h; y++ {
		if y > h/2 && y < h/2+5 {
			continue // blank lines → Z
		}
		for x := 0; x < w; x++ {
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 || x == y*w/h {
				img.Pix[y*img.Stride+x] = 0
			}
		}
	}
	return img
}

func media(t *testing.T, id string) *driver.Media {
	t.Helper()
	m, ok := LookupMedia(id)
	if !ok {
		t.Fatalf("no media %s", id)
	}
	return &m
}

func TestEncodeGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		media string
		pages []*image.Gray
		cut   string
	}{
		{"6mm_one_page", "tze-6", []*image.Gray{testPage(64, 80)}, ""},
		{"12mm_one_page", "tze-12", []*image.Gray{testPage(150, 80)}, ""},
		{"24mm_one_page", "tze-24", []*image.Gray{testPage(320, 80)}, ""},
		{"36mm_one_page", "tze-36", []*image.Gray{testPage(384, 80)}, ""},
		{"24mm_three_pages_half", "tze-24", []*image.Gray{testPage(320, 60), testPage(320, 60), testPage(320, 60)}, "half"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := New().Encode(context.Background(), &buf, tc.pages, &driver.Job{Cut: tc.cut}, media(t, tc.media)); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", tc.name+".bin")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
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

// pageLines splits one page block into its raster commands (G… or Z) and the terminator.
func pageLines(t *testing.T, page []byte) (lines [][]byte, end byte) {
	t.Helper()
	for i := 0; i < len(page); {
		switch page[i] {
		case 'Z':
			lines = append(lines, nil)
			i++
		case 'G':
			n := int(page[i+1]) | int(page[i+2])<<8
			lines = append(lines, page[i+3:i+3+n])
			i += 3 + n
		default:
			if i != len(page)-1 {
				t.Fatalf("unexpected byte %02x at %d of %d", page[i], i, len(page))
			}
			return lines, page[i]
		}
	}
	t.Fatal("page has no terminator")
	return nil, 0
}

func TestLinePinsOutsidePrintAreaZero(t *testing.T) {
	black := image.NewGray(image.Rect(0, 0, 64, 60)) // all black
	_, pages, err := New().EncodePages([]*image.Gray{black}, &driver.Job{}, media(t, "tze-6"))
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := pageLines(t, pages[0])
	for i, l := range lines {
		if len(l) != 28 {
			t.Fatalf("line %d is %d bytes, want 28", i, len(l))
		}
		for pin := range 28 * 8 {
			on := l[pin/8]&(0x80>>(pin%8)) != 0
			if inArea := pin >= 160 && pin < 224; on != inArea {
				t.Fatalf("line %d pin %d on=%v", i, pin, on)
			}
		}
	}
}

func TestEncodePagesSplit(t *testing.T) {
	pages := []*image.Gray{testPage(320, 60), testPage(320, 60), testPage(320, 60)}
	header, per, err := New().EncodePages(pages, &driver.Job{}, media(t, "tze-24"))
	if err != nil {
		t.Fatal(err)
	}
	if len(per) != 3 {
		t.Fatalf("%d page blocks", len(per))
	}
	for i, p := range per {
		want := byte(0x0c)
		if i == 2 {
			want = 0x1a
		}
		if _, end := pageLines(t, p); end != want {
			t.Errorf("page %d ends %02x, want %02x", i+1, end, want)
		}
	}
	var buf bytes.Buffer
	if err := New().Encode(context.Background(), &buf, pages, &driver.Job{}, media(t, "tze-24")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), bytes.Join(append([][]byte{header}, per...), nil)) {
		t.Error("Encode != header + pages")
	}
	wantHeader := append(make([]byte, invalidateN),
		0x1b, '@',
		0x1b, 'i', 'c', 0xf4, 0, 24, 0, 0,
		0x1b, 'i', 'M', 0x40,
		0x1b, 'i', 'K', 0x08,
		0x1b, 'i', 'd', margin, 0,
		'M', 0,
		0x1b, 'i', 'R', 1)
	if !bytes.Equal(header, wantHeader) {
		t.Errorf("header\n% x\nwant\n% x", header, wantHeader)
	}
}

func TestCutModes(t *testing.T) {
	for cut, mk := range map[string][2]byte{
		"": {0x40, 0x08}, "per_label": {0x40, 0x08}, "last": {0x00, 0x28}, "none": {0x00, 0x08}, "half": {0x40, 0x0c},
	} {
		header, _, err := New().EncodePages([]*image.Gray{white(320, 60)}, &driver.Job{Cut: cut}, media(t, "tze-24"))
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.Index(header, []byte{0x1b, 'i', 'M'})
		j := bytes.Index(header, []byte{0x1b, 'i', 'K'})
		if header[i+3] != mk[0] || header[j+3] != mk[1] {
			t.Errorf("cut %q: M=%02x K=%02x, want %02x %02x", cut, header[i+3], header[j+3], mk[0], mk[1])
		}
	}
}

func TestEncodePadsShortLabel(t *testing.T) {
	_, pages, err := New().EncodePages([]*image.Gray{testPage(320, 20)}, &driver.Job{}, media(t, "tze-24"))
	if err != nil {
		t.Fatal(err)
	}
	if lines, _ := pageLines(t, pages[0]); len(lines) != minLines {
		t.Errorf("%d lines, want %d", len(lines), minLines)
	}
}

func TestValidate(t *testing.T) {
	d := New()
	var de *driver.Error
	if err := d.Validate([]*image.Gray{white(384, 60)}, &driver.Job{}, media(t, "tze-24")); !errors.As(err, &de) || de.Code != driver.MediaMismatch {
		t.Errorf("36 mm page on 24 mm tape: %v", err)
	}
	if err := d.Validate([]*image.Gray{white(320, maxLines+1)}, &driver.Job{}, media(t, "tze-24")); !errors.As(err, &de) || de.Code != driver.Unsupported {
		t.Errorf("label over 1 m: %v", err)
	}
	if err := d.Validate(nil, &driver.Job{}, media(t, "tze-24")); err == nil {
		t.Error("no pages accepted")
	}
	if err := d.Validate([]*image.Gray{white(320, 60)}, &driver.Job{Cut: "zigzag"}, media(t, "tze-24")); err == nil {
		t.Error("bad cut mode accepted")
	}
	if err := d.Validate([]*image.Gray{white(306, 60)}, &driver.Job{}, &driver.Media{ID: "dk-22210"}); err == nil {
		t.Error("QL media accepted")
	}
}

// scripted replays status replies, one per Read of 32 bytes; then reads time out.
type scripted struct{ replies [][]byte }

func (s *scripted) Open(context.Context) error  { return nil }
func (s *scripted) Write(p []byte) (int, error) { return len(p), nil }
func (s *scripted) Close() error                { return nil }
func (s *scripted) Kind() string                { return "test" }
func (s *scripted) Read(p []byte) (int, error) {
	if len(s.replies) == 0 {
		time.Sleep(20 * time.Millisecond)
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, s.replies[0])
	s.replies = s.replies[1:]
	return n, nil
}

func TestWaitPageSkipsPhaseChange(t *testing.T) {
	phase := reply(func(b []byte) { b[18], b[19] = 0x06, 0x01 })
	done := reply(func(b []byte) { b[18] = 0x01 })
	coverOpen := reply(func(b []byte) { b[18], b[9] = 0x02, 0x10 })

	if err := WaitPage(context.Background(), &scripted{[][]byte{phase, phase, done}}, time.Second); err != nil {
		t.Errorf("phase, phase, completed: %v", err)
	}
	var de *driver.Error
	if err := WaitPage(context.Background(), &scripted{[][]byte{phase, coverOpen}}, time.Second); !errors.As(err, &de) || de.Code != driver.CoverOpen {
		t.Errorf("error status: %v", err)
	}
	start := time.Now()
	if err := WaitPage(context.Background(), &scripted{[][]byte{phase}}, 200*time.Millisecond); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("no completion: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("deadline ignored: %v", time.Since(start))
	}
}

func TestStatus(t *testing.T) {
	s, err := New().Status(context.Background(), &scripted{[][]byte{realReply()}})
	if err != nil || s.Code != driver.OK || s.Media.ID != "tze-36" {
		t.Fatalf("%+v %v", s, err)
	}
}
