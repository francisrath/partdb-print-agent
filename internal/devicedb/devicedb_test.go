package devicedb_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
)

func TestDefaultLoads(t *testing.T) {
	db := devicedb.Default()
	if len(db.Models("brother")) < 4 {
		t.Fatalf("models: %v", db.Models("brother"))
	}
}

// The database must describe the printers exactly as the code's tables did before it existed
// (phase 1: no behaviour change). Snapshot of brotherql/brotherpt LookupMedia, 2026-11-18.
func TestParityWithTables(t *testing.T) {
	db := devicedb.Default()
	for _, want := range []devicedb.Media{
		{ID: "dk-22214", Name: "12 mm continuous", Kind: "continuous", WidthMM: 12, LengthMM: 0, PrintableWidthDots: 106, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-22210", Name: "29 mm continuous", Kind: "continuous", WidthMM: 29, LengthMM: 0, PrintableWidthDots: 306, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-22225", Name: "38 mm continuous", Kind: "continuous", WidthMM: 38, LengthMM: 0, PrintableWidthDots: 413, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-22223", Name: "50 mm continuous", Kind: "continuous", WidthMM: 50, LengthMM: 0, PrintableWidthDots: 554, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-n55224", Name: "54 mm continuous", Kind: "continuous", WidthMM: 54, LengthMM: 0, PrintableWidthDots: 590, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-22205", Name: "62 mm continuous", Kind: "continuous", WidthMM: 62, LengthMM: 0, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 0, DPI: 300},
		{ID: "dk-11204", Name: "17×54 mm die-cut", Kind: "die_cut", WidthMM: 17, LengthMM: 54, PrintableWidthDots: 165, WidthDots: 0, PrintableLengthDots: 566, DPI: 300},
		{ID: "dk-11201", Name: "29×90 mm die-cut", Kind: "die_cut", WidthMM: 29, LengthMM: 90, PrintableWidthDots: 306, WidthDots: 0, PrintableLengthDots: 991, DPI: 300},
		{ID: "dk-11208", Name: "38×90 mm die-cut", Kind: "die_cut", WidthMM: 38, LengthMM: 90, PrintableWidthDots: 413, WidthDots: 0, PrintableLengthDots: 991, DPI: 300},
		{ID: "dk-11209", Name: "62×29 mm die-cut", Kind: "die_cut", WidthMM: 62, LengthMM: 29, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 271, DPI: 300},
		{ID: "dk-11202", Name: "62×100 mm die-cut", Kind: "die_cut", WidthMM: 62, LengthMM: 100, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 1109, DPI: 300},
		{ID: "tze-6", Name: "6 mm tape", Kind: "continuous", WidthMM: 6, LengthMM: 0, PrintableWidthDots: 64, WidthDots: 84, PrintableLengthDots: 0, DPI: 360},
		{ID: "tze-9", Name: "9 mm tape", Kind: "continuous", WidthMM: 9, LengthMM: 0, PrintableWidthDots: 106, WidthDots: 128, PrintableLengthDots: 0, DPI: 360},
		{ID: "tze-12", Name: "12 mm tape", Kind: "continuous", WidthMM: 12, LengthMM: 0, PrintableWidthDots: 150, WidthDots: 170, PrintableLengthDots: 0, DPI: 360},
		{ID: "tze-18", Name: "18 mm tape", Kind: "continuous", WidthMM: 18, LengthMM: 0, PrintableWidthDots: 234, WidthDots: 256, PrintableLengthDots: 0, DPI: 360},
		{ID: "tze-24", Name: "24 mm tape", Kind: "continuous", WidthMM: 24, LengthMM: 0, PrintableWidthDots: 320, WidthDots: 340, PrintableLengthDots: 0, DPI: 360},
		{ID: "tze-36", Name: "36 mm tape", Kind: "continuous", WidthMM: 36, LengthMM: 0, PrintableWidthDots: 384, WidthDots: 512, PrintableLengthDots: 0, DPI: 360},
	} {
		got, ok := db.Media(want.ID)
		got.OffsetPins, got.EdgeMM = 0, 0 // new fields, checked below
		if !ok || got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", want.ID, got, want)
		}
	}
	for id, pins := range map[string]int{"dk-22214": 29, "dk-22210": 6, "dk-n55224": 0, "dk-11202": 12, "tze-6": 160, "tze-24": 32, "tze-36": 0} {
		if m, _ := db.Media(id); m.OffsetPins != pins {
			t.Errorf("%s offset pins %d, want %d", id, m.OffsetPins, pins)
		}
	}
	if m, _ := db.Media("tze-36"); m.EdgeMM < 4.5 || m.EdgeMM > 4.53 {
		t.Errorf("tze-36 edge %v mm, want ~4.52", m.EdgeMM)
	}
}

func TestModelLookup(t *testing.T) {
	db := devicedb.Default()
	if m, ok := db.Model("QL-810W"); !ok || m.Driver != "brother" || m.Dialect != "ql" || m.DPI != 300 || m.HeadPins != 720 {
		t.Errorf("QL-810W: %+v %v", m, ok)
	}
	if m, ok := db.ModelByStatusCode("brother", 0x39); !ok || m.ID != "QL-810W" {
		t.Errorf("status code 0x39: %+v %v", m, ok)
	}
	m, ok := db.Model("PT-9500PC")
	if !ok || m.Dialect != "pt-cbp" || m.DPI != 360 || m.HeadPins != 384 || len(m.Match.USB) != 1 || m.Match.USB[0] != "04f9:200f" {
		t.Fatalf("PT-9500PC: %+v %v", m, ok)
	}
	if got := len(db.MediaFor(m)); got != 6 {
		t.Errorf("PT-9500PC media: %d", got)
	}
	if _, ok := db.Model("QL-9000"); ok {
		t.Error("unknown model found")
	}
}

func TestLint(t *testing.T) {
	media := "media:\n  - {id: tape-a, name: a, kind: continuous, width_mm: 6, printable_width_dots: 64, dpi: 360}\n"
	model := func(id, usb, med string) string {
		return "models:\n  - {id: " + id + ", driver: brother, dialect: pt-cbp, dpi: 360, head_pins: 384, transports: [usb], match: {usb: [\"" + usb + "\"]}, media: [" + med + "]}\n"
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  []string
	}{
		"duplicate usb": {map[string]string{"data/m.yaml": media, "data/a.yaml": model("A", "04f9:0001", "tape-a"), "data/b.yaml": model("B", "04f9:0001", "tape-a")}, []string{"04f9:0001", "A", "B"}},
		"unknown media": {map[string]string{"data/m.yaml": media, "data/a.yaml": model("A", "04f9:0001", "tape-x")}, []string{"A", "tape-x"}},
		"no dialect":    {map[string]string{"data/a.yaml": "models:\n  - {id: A, driver: brother}\n"}, []string{"A", "dialect"}},
	} {
		fsys := fstest.MapFS{}
		for p, c := range tc.files {
			fsys[p] = &fstest.MapFile{Data: []byte(c)}
		}
		_, err := devicedb.Load(fsys)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q doesn't name %q", name, err, w)
			}
		}
	}
}
