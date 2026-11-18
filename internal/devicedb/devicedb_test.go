package devicedb_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherpt"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherql"
)

func TestDefaultLoads(t *testing.T) {
	db := devicedb.Default()
	if len(db.Models("brother")) < 4 {
		t.Fatalf("models: %v", db.Models("brother"))
	}
}

// The database must describe the printers exactly as the code's tables did (phase 1: no behaviour change).
func TestParityWithTables(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct {
		ids    []string
		lookup func(string) (driver.Media, bool)
	}{
		{[]string{"dk-22214", "dk-22210", "dk-22225", "dk-22223", "dk-n55224", "dk-22205", "dk-11204", "dk-11201", "dk-11208", "dk-11209", "dk-11202"}, brotherql.LookupMedia},
		{[]string{"tze-6", "tze-9", "tze-12", "tze-18", "tze-24", "tze-36"}, brotherpt.LookupMedia},
	} {
		for _, id := range tc.ids {
			want, _ := tc.lookup(id)
			got, ok := db.Media(id)
			got.OffsetPins, got.EdgeMM = 0, 0 // new fields, checked below
			if !ok || got != want {
				t.Errorf("%s:\n got %+v\nwant %+v", id, got, want)
			}
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
