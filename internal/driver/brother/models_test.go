package brother

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

// Models added from Brother's references without hardware (2026-11-20): golden bytes pin the
// output; the structural tests below say what the bytes must mean per reference.

func modelMedia(t *testing.T, model, id string) (*devicedb.Model, devicedb.Media) {
	t.Helper()
	db := devicedb.Default()
	m, ok := db.Model(model)
	if !ok {
		t.Fatalf("no model %s", model)
	}
	for _, md := range db.MediaFor(m) {
		if md.ID == id {
			return m, md
		}
	}
	t.Fatalf("%s takes no %s", model, id)
	return nil, devicedb.Media{}
}

func encodeFor(t *testing.T, model, media string, pages []*image.Gray, cut string) []byte {
	t.Helper()
	m, md := modelMedia(t, model, media)
	d, err := driver.New(m, driver.Options{NoSNMP: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := d.Encode(&buf, &driver.Job{Pages: pages, Media: &md, Cut: cut}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestModelsEncodeGolden(t *testing.T) {
	for _, tc := range []struct {
		name, model, media string
		pages              []*image.Gray
		cut                string
	}{
		{"ql1100_29mm", "QL-1100", "dk-22210", []*image.Gray{testPage(306, 100)}, ""},
		{"ql1100_103mm", "QL-1100", "dk-22246", []*image.Gray{testPage(1200, 320)}, ""},
		{"ql1100_102x51_two", "QL-1100", "dk-11240", []*image.Gray{testPage(1164, 526), testPage(1164, 526)}, "last"},
		{"ql700_62mm", "QL-700", "dk-22205", []*image.Gray{testPage(696, 160)}, ""},
		{"ptp750w_24mm_two_half", "PT-P750W", "tze-24", []*image.Gray{testPage(128, 90), testPage(128, 20)}, "half"},
		{"ptp710bt_12mm", "PT-P710BT", "tze-12", []*image.Gray{testPage(70, 80)}, ""},
		{"ptp900_36mm", "PT-P900", "tze-36", []*image.Gray{testPage(454, 120)}, "per_label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeFor(t, tc.model, tc.media, tc.pages, tc.cut)
			golden := filepath.Join("testdata", "models_"+tc.name+".bin")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from %s", golden)
			}
		})
	}
}

// qlLines splits a QL page stream after the header into its raster lines (uncompressed).
func qlLines(t *testing.T, b []byte, compressed bool) [][]byte {
	t.Helper()
	i := bytes.Index(b, []byte{'M', 2})
	if !compressed {
		i = bytes.Index(b, []byte{0x1b, 'i', 'd'}) + 5 // QL-700: no M command
	} else {
		i += 2
	}
	var out [][]byte
	for i < len(b) && b[i] == 'g' {
		n := int(b[i+2])
		line := b[i+3 : i+3+n]
		if compressed {
			line = unpackbits(line)
		}
		out = append(out, line)
		i += 3 + n
	}
	return out
}

// A wide QL sends 162-byte lines; a 29 mm roll sits on pins 50–355 (QL-1100 reference p.19),
// the image's rightmost column on pin 50.
func TestQLWideLines(t *testing.T) {
	img := white(306, 1)
	img.Pix[305] = 0 // rightmost column
	img.Pix[0] = 0   // leftmost
	b := encodeFor(t, "QL-1100", "dk-22210", []*image.Gray{img}, "")
	if !bytes.Contains(b, []byte{0x1b, 'i', 'a', 1}) || !bytes.Contains(b, []byte{0x1b, 'i', 'z', 0xce, 0x0a, 29, 0}) {
		t.Fatalf("header: % x", b[400:440])
	}
	lines := qlLines(t, b, true)
	if len(lines) != 301 { // padded to the QL-1100's 301-dot minimum
		t.Fatalf("%d lines", len(lines))
	}
	var line []byte
	for _, l := range lines {
		if !bytes.Equal(l, make([]byte, 162)) {
			line = l
		}
	}
	if len(line) != 162 || line[50/8] != 0x80>>(50%8) || line[355/8] != 0x80>>(355%8) {
		t.Fatalf("line % x", line)
	}
	// 103.6 mm roll: Brother's "104 mm" in ESC i z
	b = encodeFor(t, "QL-1110NWB", "dk-22246", []*image.Gray{white(1200, 400)}, "")
	if !bytes.Contains(b, []byte{0x1b, 'i', 'z', 0xce, 0x0a, 104, 0}) {
		t.Error("103 mm roll not sent as 104 mm")
	}
}

// The QL-700 has no ESC i a and no M command; lines go uncompressed (QL-500…1060N reference p.3).
func TestQL700Legacy(t *testing.T) {
	b := encodeFor(t, "QL-700", "dk-22210", []*image.Gray{testPage(306, 200)}, "")
	if bytes.Contains(b, []byte{0x1b, 'i', 'a'}) || bytes.Contains(b, []byte{0x1b, 'i', 'd', 35, 0, 'M'}) {
		t.Error("QL-700 got ESC i a or M")
	}
	lines := qlLines(t, b, false)
	if len(lines) != 200 || len(lines[0]) != 90 {
		t.Fatalf("%d lines of %d bytes", len(lines), len(lines[0]))
	}
}

// ptRasterPage reads one page of a pt-raster stream from its ESC i z: the print information,
// the mode bytes and the raster lines unpacked to head width.
func ptRasterPage(t *testing.T, b []byte, lineBytes int) (z, mode []byte, lines [][]byte, end byte, rest []byte) {
	t.Helper()
	i := bytes.Index(b, []byte{0x1b, 'i', 'z'})
	if i < 0 {
		t.Fatal("no ESC i z")
	}
	z = b[i+3 : i+13]
	j := bytes.Index(b[i:], []byte{'M', 2})
	mode = b[i+13 : i+j]
	k := i + j + 2
	for b[k] == 'G' || b[k] == 'Z' {
		if b[k] == 'Z' {
			lines = append(lines, make([]byte, lineBytes))
			k++
			continue
		}
		n := int(binary.LittleEndian.Uint16(b[k+1:]))
		lines = append(lines, unpackbits(b[k+3:k+3+n]))
		k += 3 + n
	}
	return z, mode, lines, b[k], b[k+1:]
}

func TestPTRasterEncode(t *testing.T) {
	// PT-P750W, 24 mm: 128 pins, the tape's 128 dots on pins 0–127; half cut between labels
	img := white(128, 40)
	img.Pix[127] = 0 // rightmost column → pin 0
	img.Pix[0] = 0   // leftmost → pin 127
	b := encodeFor(t, "PT-P750W", "tze-24", []*image.Gray{img, white(128, 40)}, "half")
	if !bytes.HasPrefix(b, append(make([]byte, 200), 0x1b, '@', 0x1b, 'i', 'a', 1, 0x1b, 'i', 'z')) {
		t.Fatalf("start % x", b[190:215])
	}
	z, mode, lines, end, rest := ptRasterPage(t, b, 16)
	if !bytes.Equal(z, []byte{0x84, 0, 24, 0, 40, 0, 0, 0, 0, 0}) {
		t.Errorf("print information % x", z)
	}
	wantMode := []byte{0x1b, 'i', 'M', 0x40, 0x1b, 'i', 'A', 1, 0x1b, 'i', 'K', 0x04, 0x1b, 'i', 'd', 14, 0}
	if !bytes.Equal(mode, wantMode) {
		t.Errorf("modes % x, want % x", mode, wantMode)
	}
	if len(lines) != 40 || len(lines[0]) != 16 || lines[0][0] != 0x80 || lines[0][15] != 0x01 || end != 0x0c {
		t.Errorf("%d lines, first % x, end %02x", len(lines), lines[0], end)
	}
	z, _, _, end, _ = ptRasterPage(t, rest, 16)
	if z[8] != 1 || end != 0x1a {
		t.Errorf("second page: page byte %d, end %02x", z[8], end)
	}

	// PT-P710BT: status notification on, no ESC i A; a short label padded to 31 lines
	b = encodeFor(t, "PT-P710BT", "tze-12", []*image.Gray{white(70, 10)}, "")
	if !bytes.Contains(b, []byte{0x1b, 'i', 'a', 1, 0x1b, 'i', '!', 0}) || bytes.Contains(b, []byte{0x1b, 'i', 'A'}) {
		t.Error("PT-P710BT: want ESC i ! 0, no ESC i A")
	}
	if z, _, lines, _, _ := ptRasterPage(t, b, 16); len(lines) != 31 || z[4] != 31 || z[8] != 0 {
		t.Errorf("PT-P710BT: %d lines, print information % x", len(lines), z)
	}

	// PT-P900, 12 mm: 560 pins, print area on pins 213–362 (p.20); the only page is marked last
	img = white(150, 60)
	img.Pix[149] = 0
	img.Pix[0] = 0
	b = encodeFor(t, "PT-P900", "tze-12", []*image.Gray{img}, "")
	z, mode, lines, _, _ = ptRasterPage(t, b, 70)
	if z[2] != 12 || z[8] != 2 || !bytes.Contains(mode, []byte{0x1b, 'i', 'K', 0x00}) {
		t.Errorf("PT-P900: print information % x, modes % x", z, mode)
	}
	if l := lines[0]; len(l) != 70 || l[213/8] != 0x80>>(213%8) || l[362/8] != 0x80>>(362%8) {
		t.Errorf("PT-P900 line % x", l)
	}
}

func TestPTRasterStatus(t *testing.T) {
	m, _ := devicedb.Default().Model("PT-P710BT")
	d := newPTRaster(m, driver.Options{NoSNMP: true})
	reply := func(edit func(b []byte)) []byte {
		b := make([]byte, 32)
		b[0], b[1], b[2], b[3], b[4] = 0x80, 0x20, 'B', '0', 0x68
		b[10], b[11] = 24, 0x01
		edit(b)
		return b
	}
	for _, tc := range []struct {
		name         string
		edit         func(b []byte)
		code         driver.Code
		model, media string
		typ          string
		done         bool
	}{
		{"ok", func([]byte) {}, driver.OK, "PT-P750W", "tze-24", "laminated", false},
		{"own model when code unknown", func(b []byte) { b[4] = 0x7f }, driver.OK, "PT-P710BT", "tze-24", "laminated", false},
		{"no tape", func(b []byte) { b[8], b[10], b[11] = 0x01, 0, 0 }, driver.NoMedia, "PT-P750W", "", "", false},
		{"cover", func(b []byte) { b[9] = 0x10 }, driver.CoverOpen, "PT-P750W", "tze-24", "laminated", false},
		{"overheat", func(b []byte) { b[9] = 0x20 }, driver.Overheat, "PT-P750W", "tze-24", "laminated", false},
		{"batteries", func(b []byte) { b[8] = 0x08 }, driver.Unknown, "PT-P750W", "tze-24", "laminated", false},
		{"heat-shrink is no TZe", func(b []byte) { b[11] = 0x11 }, driver.OK, "PT-P750W", "unknown", "heat-shrink 2:1", false},
		{"unsupported width", func(b []byte) { b[10] = 36 }, driver.OK, "PT-P750W", "unknown", "laminated", false},
		{"completed", func(b []byte) { b[18] = 0x01 }, driver.OK, "PT-P750W", "tze-24", "laminated", true},
		{"error status", func(b []byte) { b[18] = 0x02 }, driver.Unknown, "PT-P750W", "tze-24", "laminated", false},
		{"flexible ID tape", func(b []byte) { b[11] = 0x14 }, driver.OK, "PT-P750W", "tze-24", "flexible ID", false},
		{"satin tape", func(b []byte) { b[11] = 0x15 }, driver.OK, "PT-P750W", "tze-24", "satin", false},
		{"FLe labels are no tape", func(b []byte) { b[10], b[11] = 21, 0x13 }, driver.OK, "PT-P750W", "unknown", "FLe label", false},
	} {
		s, err := d.parse(reply(tc.edit))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if s.Code != tc.code || s.Model != tc.model || mediaID(s.Media) != tc.media || s.MediaType != tc.typ || s.Done != tc.done {
			t.Errorf("%s: %+v media %s", tc.name, s, mediaID(s.Media))
		}
	}
	if _, err := d.parse(make([]byte, 32)); err == nil {
		t.Error("garbage parsed")
	}
}

// cut "none" leaves the last label in the printer uncut: no auto cut, chain printing (ESC i K
// bit 3 off; on, it feeds and cuts after the last label, PT-E550W reference p.33).
func TestPTRasterCutNone(t *testing.T) {
	for _, model := range []string{"PT-E550W", "PT-P750W", "PT-P710BT", "PT-P700", "PT-E500", "PT-P900", "PT-P900W", "PT-P950NW", "PT-P910BT"} {
		b := encodeFor(t, model, "tze-12", []*image.Gray{white(10, 40)}, "none")
		_, mode, _, _, _ := ptRasterPage(t, b, 16)
		if !bytes.HasPrefix(mode, []byte{0x1b, 'i', 'M', 0x00, 0x1b, 'i', 'K', 0x00}) {
			t.Errorf("%s: modes % x", model, mode)
		}
	}
}

// pageTape answers each print command with "printing completed" once the page is fully sent,
// recording what was written before each completion was read.
type pageTape struct {
	kind    string
	written bytes.Buffer
	pending [][]byte
	seen    []int // bytes written when each reply was handed out
}

func (p *pageTape) Open(context.Context) error { return nil }
func (p *pageTape) Close() error               { return nil }
func (p *pageTape) Kind() string               { return p.kind }
func (p *pageTape) Write(b []byte) (int, error) {
	p.written.Write(b)
	for range bytes.Count(b, []byte{0x0c}) + bytes.Count(b, []byte{0x1a}) {
		r := make([]byte, 32)
		r[0], r[1], r[2], r[3], r[4], r[10], r[11], r[18] = 0x80, 0x20, 'B', '0', 0x68, 24, 0x01, 0x01
		p.pending = append(p.pending, r)
	}
	return len(b), nil
}
func (p *pageTape) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	p.seen = append(p.seen, p.written.Len())
	n := copy(b, p.pending[0])
	p.pending = p.pending[1:]
	return n, nil
}

// Over USB/Bluetooth each label is sent only after the previous one reported "printing
// completed" ("(2) through (4) are repeated", PT-E550W reference p.4 and flow chart 5.1).
func TestPTRasterPrintWaitsPerPage(t *testing.T) {
	m, md := modelMedia(t, "PT-P750W", "tze-24")
	d := newPTRaster(m, driver.Options{NoSNMP: true})
	pages := []*image.Gray{white(128, 40), white(128, 40), white(128, 40)}
	job := &driver.Job{Pages: pages, Media: &md, Status: &driver.Status{Code: driver.OK, Via: "usb"}}
	var want bytes.Buffer
	if err := d.Encode(&want, job); err != nil {
		t.Fatal(err)
	}
	tr := &pageTape{kind: "usb"}
	res, err := d.Print(context.Background(), tr, job)
	if err != nil || !res.Confirmed {
		t.Fatalf("%+v %v", res, err)
	}
	if !bytes.Equal(tr.written.Bytes(), want.Bytes()) {
		t.Error("sent bytes differ from Encode")
	}
	// one completion read per page, each before the next page was written
	if len(tr.seen) != 3 || tr.seen[0] >= tr.seen[1] || tr.seen[1] >= tr.seen[2] || tr.seen[2] != want.Len() {
		t.Errorf("completions read at %v of %d bytes", tr.seen, want.Len())
	}
	if first := bytes.IndexByte(want.Bytes()[200:], 0x0c) + 200 + 1; tr.seen[0] != first {
		t.Errorf("first completion read after %d bytes, page 1 ends at %d", tr.seen[0], first)
	}
}

// A P-touch on USB or Bluetooth that gave no status can't be paced: one label still prints,
// a batch is refused before anything is sent. On the network the job goes at once as before.
func TestPTRasterSilentPrinter(t *testing.T) {
	m, md := modelMedia(t, "PT-P710BT", "tze-12")
	d := newPTRaster(m, driver.Options{NoSNMP: true})
	two := []*image.Gray{white(70, 40), white(70, 40)}
	for _, kind := range []string{"usb", "bt"} {
		tr := &pageTape{kind: kind}
		_, err := d.Print(context.Background(), tr, &driver.Job{Pages: two, Media: &md})
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != driver.CommsError || !strings.Contains(de.Detail, "one label per job") || tr.written.Len() != 0 {
			t.Errorf("%s, two labels: %v, %d bytes sent", kind, err, tr.written.Len())
		}
		tr = &pageTape{kind: kind}
		if res, err := d.Print(context.Background(), tr, &driver.Job{Pages: two[:1], Media: &md}); err != nil || tr.written.Len() == 0 || res.Confirmed {
			t.Errorf("%s, one label: %+v %v", kind, res, err)
		}
	}
	tr := &pageTape{kind: "tcp"}
	if _, err := d.Print(context.Background(), tr, &driver.Job{Pages: two, Media: &md}); err != nil || tr.written.Len() == 0 {
		t.Errorf("tcp, two labels: %v", err)
	}
}
