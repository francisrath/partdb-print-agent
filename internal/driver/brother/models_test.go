package brother

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
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
