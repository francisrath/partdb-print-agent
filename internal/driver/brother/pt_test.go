package brother

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// realReply is what a PT-9500PC with 36 mm laminated tape answered to ESC i S (2026-11-18).
func realReply() []byte {
	b := make([]byte, 32)
	copy(b, []byte{0x80, 0x20, 0x42, 0x30, 0x4a, 0x30, 0, 0, 0, 0, 0x24, 0x01})
	return b
}

func reply(edit func(b []byte)) []byte {
	b := realReply()
	edit(b)
	return b
}

func TestPTParseStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []byte
		code      driver.Code
		detail    string
		media     string
		mediaType string
		typ       byte
		done      bool
	}{
		{"real reply", realReply(), driver.OK, "", "tze-36", "laminated", 0x00, false},
		{"no media", reply(func(b []byte) { b[8] = 0x01 }), driver.NoMedia, "no media", "tze-36", "laminated", 0, false},
		{"end of media", reply(func(b []byte) { b[8] = 0x02 }), driver.NoMedia, "end of media", "tze-36", "laminated", 0, false},
		{"cutter jam", reply(func(b []byte) { b[8] = 0x04 }), driver.CutterJam, "cutter jam", "tze-36", "laminated", 0, false},
		{"replace media", reply(func(b []byte) { b[9] = 0x01 }), driver.MediaMismatch, "replace media", "tze-36", "laminated", 0, false},
		{"expansion buffer full", reply(func(b []byte) { b[9] = 0x02 }), driver.CommsError, "", "tze-36", "laminated", 0, false},
		{"transmission error", reply(func(b []byte) { b[9] = 0x04 }), driver.CommsError, "", "tze-36", "laminated", 0, false},
		{"transmission buffer full", reply(func(b []byte) { b[9] = 0x08 }), driver.CommsError, "", "tze-36", "laminated", 0, false},
		{"cover open", reply(func(b []byte) { b[9] = 0x10 }), driver.CoverOpen, "cover open", "tze-36", "laminated", 0, false},
		{"printing completed", reply(func(b []byte) { b[18] = 0x01 }), driver.OK, "", "tze-36", "laminated", 0x01, true},
		{"phase change", reply(func(b []byte) { b[18] = 0x06; b[19] = 0x01 }), driver.OK, "", "tze-36", "laminated", 0x06, false},
		{"hg tape", reply(func(b []byte) { b[10] = 12; b[11] = 0x09 }), driver.OK, "", "tze-12", "hg", 0, false},
		{"non-laminated", reply(func(b []byte) { b[10] = 18; b[11] = 0x03 }), driver.OK, "", "tze-18", "non-laminated", 0, false},
		{"unknown width", reply(func(b []byte) { b[10] = 5 }), driver.OK, "", "unknown", "laminated", 0, false},
		{"no tape", reply(func(b []byte) { b[8] = 0x01; b[10] = 0; b[11] = 0 }), driver.NoMedia, "no media", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, typ, err := newPTForTest(t).parseStatus(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if s.Code != tc.code || (tc.detail != "" && s.Detail != tc.detail) {
				t.Errorf("code %s %q, want %s %q", s.Code, s.Detail, tc.code, tc.detail)
			}
			if got := mediaID(s.Media); got != tc.media {
				t.Errorf("media %q, want %q", got, tc.media)
			}
			if s.MediaType != tc.mediaType {
				t.Errorf("media type %q, want %q", s.MediaType, tc.mediaType)
			}
			if typ != tc.typ || s.Done != tc.done {
				t.Errorf("type %02x done %v, want %02x %v", typ, s.Done, tc.typ, tc.done)
			}
			if s.Model != "PT-9500PC" {
				t.Errorf("model %q", s.Model)
			}
		})
	}
	if s, _, _ := newPTForTest(t).parseStatus(reply(func(b []byte) { b[10] = 5 })); s.Media.WidthMM != 5 {
		t.Errorf("unknown media width %v, want 5", s.Media.WidthMM)
	}
}

func TestPTParseStatusRejectsGarbage(t *testing.T) {
	for name, in := range map[string][]byte{
		"bad header": reply(func(b []byte) { b[0] = 0x81 }),
		"short":      realReply()[:31],
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := newPTForTest(t).parseStatus(in)
			if e, ok := err.(*driver.Error); !ok || e.Code != driver.CommsError {
				t.Fatalf("err = %v, want COMMS_ERROR", err)
			}
		})
	}
}

func TestPTMediaForWidth(t *testing.T) {
	d := newPTForTest(t)
	for mm, id := range map[int]string{6: "tze-6", 12: "tze-12", 24: "tze-24", 36: "tze-36"} {
		if m, ok := d.mediaForWidth(mm); !ok || m.ID != id {
			t.Errorf("%d mm → %v %v", mm, m.ID, ok)
		}
	}
	if _, ok := d.mediaForWidth(5); ok {
		t.Error("5 mm tape known")
	}
}

func mediaID(m *driver.Media) string {
	if m == nil {
		return ""
	}
	return m.ID
}

// testPage draws a border, a diagonal and a blank band, enough to catch orientation,
// offset and zero-line changes.
func ptTestPage(w, h int) *image.Gray {
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
	m, ok := devicedb.Default().Media(id)
	if !ok {
		t.Fatalf("no media %s", id)
	}
	return &m
}

func newPTForTest(t *testing.T) *pt {
	t.Helper()
	m, _ := devicedb.Default().Model("PT-9500PC")
	d, err := driver.New(m, driver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*pt)
}

func TestPTEncodeGolden(t *testing.T) {
	for _, tc := range []struct {
		name  string
		media string
		pages []*image.Gray
		cut   string
	}{
		{"6mm_one_page", "tze-6", []*image.Gray{ptTestPage(64, 80)}, ""},
		{"12mm_one_page", "tze-12", []*image.Gray{ptTestPage(150, 80)}, ""},
		{"24mm_one_page", "tze-24", []*image.Gray{ptTestPage(320, 80)}, ""},
		{"36mm_one_page", "tze-36", []*image.Gray{ptTestPage(384, 80)}, ""},
		{"24mm_three_pages_half", "tze-24", []*image.Gray{ptTestPage(320, 60), ptTestPage(320, 60), ptTestPage(320, 60)}, "half"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := newPTForTest(t).Encode(&buf, &driver.Job{Pages: tc.pages, Cut: tc.cut, Media: media(t, tc.media)}); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "pt_"+tc.name+".bin")
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
	_, pages, err := newPTForTest(t).encodePages(&driver.Job{Pages: []*image.Gray{black}, Media: media(t, "tze-6")})
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
	pages := []*image.Gray{ptTestPage(320, 60), ptTestPage(320, 60), ptTestPage(320, 60)}
	header, per, err := newPTForTest(t).encodePages(&driver.Job{Pages: pages, Media: media(t, "tze-24")})
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
	if err := newPTForTest(t).Encode(&buf, &driver.Job{Pages: pages, Media: media(t, "tze-24")}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), bytes.Join(append([][]byte{header}, per...), nil)) {
		t.Error("Encode != header + pages")
	}
	wantHeader := append(make([]byte, 100),
		0x1b, '@',
		0x1b, 'i', 'c', 0xf4, 0, 24, 0, 0,
		0x1b, 'i', 'M', 0x40,
		0x1b, 'i', 'K', 0x00,
		0x1b, 'i', 'd', 28, 0,
		'M', 0,
		0x1b, 'i', 'R', 1)
	if !bytes.Equal(header, wantHeader) {
		t.Errorf("header\n% x\nwant\n% x", header, wantHeader)
	}
}

func TestCutModes(t *testing.T) {
	for cut, mk := range map[string][2]byte{
		"": {0x40, 0x00}, "chain": {0x40, 0x00}, "per_label": {0x40, 0x08}, "none": {0x00, 0x00}, "half": {0x00, 0x04},
	} {
		header, _, err := newPTForTest(t).encodePages(&driver.Job{Pages: []*image.Gray{white(320, 60)}, Cut: cut, Media: media(t, "tze-24")})
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
	_, pages, err := newPTForTest(t).encodePages(&driver.Job{Pages: []*image.Gray{ptTestPage(320, 20)}, Media: media(t, "tze-24")})
	if err != nil {
		t.Fatal(err)
	}
	if lines, _ := pageLines(t, pages[0]); len(lines) != 57 {
		t.Errorf("%d lines, want %d", len(lines), 57)
	}
}

func TestPTValidate(t *testing.T) {
	d := newPTForTest(t)
	var de *driver.Error
	if err := d.validate(&driver.Job{Pages: []*image.Gray{white(384, 60)}, Media: media(t, "tze-24")}); !errors.As(err, &de) || de.Code != driver.MediaMismatch {
		t.Errorf("36 mm page on 24 mm tape: %v", err)
	}
	if err := d.validate(&driver.Job{Pages: []*image.Gray{white(320, 14173+1)}, Media: media(t, "tze-24")}); !errors.As(err, &de) || de.Code != driver.Unsupported {
		t.Errorf("label over 1 m: %v", err)
	}
	if err := d.validate(&driver.Job{Pages: nil, Media: media(t, "tze-24")}); err == nil {
		t.Error("no pages accepted")
	}
	if err := d.validate(&driver.Job{Pages: []*image.Gray{white(320, 60)}, Cut: "zigzag", Media: media(t, "tze-24")}); err == nil {
		t.Error("bad cut mode accepted")
	}
	// labels go out one at a time, so the printer can't tell which is last (tested 2026-11-18)
	if err := d.validate(&driver.Job{Pages: []*image.Gray{white(320, 60)}, Cut: "last", Media: media(t, "tze-24")}); err == nil || !strings.Contains(err.Error(), "last") {
		t.Errorf("cut: last accepted: %v", err)
	}
	if err := d.validate(&driver.Job{Pages: []*image.Gray{white(306, 60)}, Media: &driver.Media{ID: "dk-22210"}}); err == nil {
		t.Error("QL media accepted")
	}
}

// scripted replays status replies, one chunk per Read; then reads time out. With
// gapAfterFirst, one read times out after the first chunk.
type scripted struct {
	replies       [][]byte
	gapAfterFirst bool
	reads         int
}

func (s *scripted) Open(context.Context) error  { return nil }
func (s *scripted) Write(p []byte) (int, error) { return len(p), nil }
func (s *scripted) Close() error                { return nil }
func (s *scripted) Kind() string                { return "test" }
func (s *scripted) Read(p []byte) (int, error) {
	s.reads++
	if s.gapAfterFirst && s.reads == 2 {
		return 0, os.ErrDeadlineExceeded
	}
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

	if err := newPTForTest(t).waitPage(context.Background(), &scripted{replies: [][]byte{phase, phase, done}}, time.Second); err != nil {
		t.Errorf("phase, phase, completed: %v", err)
	}
	var de *driver.Error
	if err := newPTForTest(t).waitPage(context.Background(), &scripted{replies: [][]byte{phase, coverOpen}}, time.Second); !errors.As(err, &de) || de.Code != driver.CoverOpen {
		t.Errorf("error status: %v", err)
	}
	start := time.Now()
	if err := newPTForTest(t).waitPage(context.Background(), &scripted{replies: [][]byte{phase}}, 200*time.Millisecond); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("no completion: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("deadline ignored: %v", time.Since(start))
	}
}

// Statuses left over from an unconfirmed job must not pass for the reply to ESC i S.
func TestStatusSkipsStaleReplies(t *testing.T) {
	stale := [][]byte{
		reply(func(b []byte) { b[18], b[19] = 0x06, 0x01 }),
		reply(func(b []byte) { b[18] = 0x01 }),
		realReply(),
	}
	tr := &scripted{replies: stale}
	s, err := newPTForTest(t).Status(context.Background(), tr)
	if err != nil || s.Done || s.Code != driver.OK {
		t.Fatalf("%+v %v", s, err)
	}
	if len(tr.replies) != 0 {
		t.Errorf("%d replies left: the stale ones weren't skipped", len(tr.replies))
	}
}

// A status split by a read timeout (a pause mid-reply) must not shift the stream.
func TestReadStatusAcrossTimeout(t *testing.T) {
	r := realReply()
	tr := &scripted{replies: [][]byte{r[:10], r[10:]}}
	tr.gapAfterFirst = true
	b, err := readReply(tr)
	if err != nil {
		t.Fatal(err)
	}
	s, typ, err := newPTForTest(t).parseStatus(b)
	if err != nil || typ != ptStatusReply || s.Media.ID != "tze-36" {
		t.Fatalf("%+v %02x %v", s, typ, err)
	}
}

func TestPTStatus(t *testing.T) {
	s, err := newPTForTest(t).Status(context.Background(), &scripted{replies: [][]byte{realReply()}})
	if err != nil || s.Code != driver.OK || s.Media.ID != "tze-36" {
		t.Fatalf("%+v %v", s, err)
	}
}

// TestColumnZeroOnLastPin pins the orientation found on hardware (2026-11-18): with image
// column x on pin offsetPins+x the label came out mirrored, so column 0 belongs on the
// print area's last pin.
func TestColumnZeroOnLastPin(t *testing.T) {
	page := white(384, 60)
	for y := range 60 {
		page.Pix[y*page.Stride] = 0 // column 0 black
	}
	_, pages, err := newPTForTest(t).encodePages(&driver.Job{Pages: []*image.Gray{page}, Media: media(t, "tze-36")})
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := pageLines(t, pages[0])
	if l := lines[0]; l[47] != 0x01 || l[0] != 0 {
		t.Errorf("column 0 → first byte %02x, last byte %02x; want pin 383 only", l[0], l[47])
	}
}

// fakeTape plays a PT-9500PC behind a transport: after each page (a write ending in 0x0C or
// 0x1A) it reports a phase change and "printing completed".
type fakeTape struct {
	writes  [][]byte
	queue   []byte
	failOn  int  // write number that fails with ENODEV (0 = never)
	goneRx  bool // reads fail with ENODEV once a page was written
	written int
}

func (f *fakeTape) Open(context.Context) error { return nil }
func (f *fakeTape) Close() error               { return nil }
func (f *fakeTape) Kind() string               { return "usb" }
func (f *fakeTape) Write(p []byte) (int, error) {
	f.written++
	if f.written == f.failOn {
		return 0, syscall.ENODEV
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	if end := p[len(p)-1]; end == 0x0c || end == 0x1a {
		f.queue = append(f.queue, reply(func(b []byte) { b[18], b[19] = 0x06, 0x01 })...)
		f.queue = append(f.queue, reply(func(b []byte) { b[18] = 0x01 })...)
	}
	return len(p), nil
}
func (f *fakeTape) Read(p []byte) (int, error) {
	if f.goneRx && len(f.writes) > 1 {
		return 0, syscall.ENODEV
	}
	if len(f.queue) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.queue)
	f.queue = f.queue[n:]
	return n, nil
}

func ptJob(t *testing.T, n int, cut, mediaType string) *driver.Job {
	pages := make([]*image.Gray, n)
	for i := range pages {
		pages[i] = ptTestPage(384, 60)
	}
	m := media(t, "tze-36")
	return &driver.Job{Pages: pages, Media: m, Cut: cut, Status: &driver.Status{Code: driver.OK, Media: m, MediaType: mediaType}}
}

func headerByte(t *testing.T, sent []byte, cmd byte) byte {
	t.Helper()
	i := bytes.Index(sent, []byte{0x1b, 'i', cmd})
	if i < 0 {
		t.Fatalf("no ESC i %c", cmd)
	}
	return sent[i+3]
}

func TestPTPrintFlow(t *testing.T) {
	for _, tc := range []struct {
		name, cut, tape string
		pages           int
		m, k            byte
		warn            bool
	}{
		{"single label: chain", "", "laminated", 1, 0x40, 0x00, false},
		{"batch on laminated: half-cut strip", "", "laminated", 3, 0x00, 0x04, false},
		{"batch on non-laminated: chain", "", "non-laminated", 2, 0x40, 0x00, false},
		{"half asked on non-laminated: chain + warning", "half", "non-laminated", 2, 0x40, 0x00, true},
		{"per_label", "per_label", "laminated", 2, 0x40, 0x08, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTape{}
			res, err := newPTForTest(t).Print(context.Background(), f, ptJob(t, tc.pages, tc.cut, tc.tape))
			if err != nil {
				t.Fatal(err)
			}
			sent := bytes.Join(f.writes, nil)
			if m, k := headerByte(t, sent, 'M'), headerByte(t, sent, 'K'); m != tc.m || k != tc.k {
				t.Errorf("M %02x K %02x, want %02x %02x", m, k, tc.m, tc.k)
			}
			if !res.Confirmed || res.Pages != tc.pages || len(f.writes) != 1+tc.pages || len(f.queue) != 0 {
				t.Errorf("result %+v, %d writes, %d bytes unread", res, len(f.writes), len(f.queue))
			}
			if (len(res.Warnings) > 0) != tc.warn {
				t.Errorf("warnings %q", res.Warnings)
			}
		})
	}
}

// A PT can't print blind: label-by-label pacing needs its replies.
func TestPTPrintNeedsStatus(t *testing.T) {
	f := &fakeTape{}
	job := ptJob(t, 1, "", "laminated")
	job.Status = nil
	_, err := newPTForTest(t).Print(context.Background(), f, job)
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError || len(f.writes) != 0 {
		t.Fatalf("err %v, %d writes", err, len(f.writes))
	}
}

func TestPTPrintDeviceGone(t *testing.T) {
	for name, f := range map[string]*fakeTape{"while sending": {failOn: 2}, "while waiting": {goneRx: true}} {
		_, err := newPTForTest(t).Print(context.Background(), f, ptJob(t, 1, "", "laminated"))
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != driver.CommsError {
			t.Errorf("%s: err = %v, want COMMS_ERROR", name, err)
		}
	}
}
