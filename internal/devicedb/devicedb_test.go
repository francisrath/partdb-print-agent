package devicedb_test

import (
	"slices"
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
// (phase 1: no behaviour change). Snapshot of brotherql/brotherpt LookupMedia and their raster
// offsets (rightPins / offsetPins), 2026-11-18: a one-pin change moves every label.
func TestParityWithTables(t *testing.T) {
	db := devicedb.Default()
	for _, want := range []devicedb.Media{
		{ID: "dk-22214", Name: "12 mm continuous", Kind: "continuous", WidthMM: 12, LengthMM: 0, PrintableWidthDots: 106, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 29},
		{ID: "dk-22210", Name: "29 mm continuous", Kind: "continuous", WidthMM: 29, LengthMM: 0, PrintableWidthDots: 306, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 6},
		{ID: "dk-22225", Name: "38 mm continuous", Kind: "continuous", WidthMM: 38, LengthMM: 0, PrintableWidthDots: 413, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 12},
		{ID: "dk-22223", Name: "50 mm continuous", Kind: "continuous", WidthMM: 50, LengthMM: 0, PrintableWidthDots: 554, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 12},
		{ID: "dk-n55224", Name: "54 mm continuous", Kind: "continuous", WidthMM: 54, LengthMM: 0, PrintableWidthDots: 590, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 0},
		{ID: "dk-22205", Name: "62 mm continuous", Kind: "continuous", WidthMM: 62, LengthMM: 0, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 0, DPI: 300, OffsetPins: 12},
		{ID: "dk-11204", Name: "17×54 mm die-cut", Kind: "die_cut", WidthMM: 17, LengthMM: 54, PrintableWidthDots: 165, WidthDots: 0, PrintableLengthDots: 566, DPI: 300, OffsetPins: 0},
		{ID: "dk-11201", Name: "29×90 mm die-cut", Kind: "die_cut", WidthMM: 29, LengthMM: 90, PrintableWidthDots: 306, WidthDots: 0, PrintableLengthDots: 991, DPI: 300, OffsetPins: 6},
		{ID: "dk-11208", Name: "38×90 mm die-cut", Kind: "die_cut", WidthMM: 38, LengthMM: 90, PrintableWidthDots: 413, WidthDots: 0, PrintableLengthDots: 991, DPI: 300, OffsetPins: 12},
		{ID: "dk-11209", Name: "62×29 mm die-cut", Kind: "die_cut", WidthMM: 62, LengthMM: 29, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 271, DPI: 300, OffsetPins: 12},
		{ID: "dk-11202", Name: "62×100 mm die-cut", Kind: "die_cut", WidthMM: 62, LengthMM: 100, PrintableWidthDots: 696, WidthDots: 0, PrintableLengthDots: 1109, DPI: 300, OffsetPins: 12},
		{ID: "tze-6", Name: "6 mm tape", Kind: "continuous", WidthMM: 6, LengthMM: 0, PrintableWidthDots: 64, WidthDots: 84, PrintableLengthDots: 0, DPI: 360, OffsetPins: 160},
		{ID: "tze-9", Name: "9 mm tape", Kind: "continuous", WidthMM: 9, LengthMM: 0, PrintableWidthDots: 106, WidthDots: 128, PrintableLengthDots: 0, DPI: 360, OffsetPins: 139},
		{ID: "tze-12", Name: "12 mm tape", Kind: "continuous", WidthMM: 12, LengthMM: 0, PrintableWidthDots: 150, WidthDots: 170, PrintableLengthDots: 0, DPI: 360, OffsetPins: 117},
		{ID: "tze-18", Name: "18 mm tape", Kind: "continuous", WidthMM: 18, LengthMM: 0, PrintableWidthDots: 234, WidthDots: 256, PrintableLengthDots: 0, DPI: 360, OffsetPins: 75},
		{ID: "tze-24", Name: "24 mm tape", Kind: "continuous", WidthMM: 24, LengthMM: 0, PrintableWidthDots: 320, WidthDots: 340, PrintableLengthDots: 0, DPI: 360, OffsetPins: 32},
		{ID: "tze-36", Name: "36 mm tape", Kind: "continuous", WidthMM: 36, LengthMM: 0, PrintableWidthDots: 384, WidthDots: 512, PrintableLengthDots: 0, DPI: 360, OffsetPins: 0},
	} {
		got, ok := db.Media(want.ID)
		got.EdgeMM = 0 // new field, checked below
		if !ok || got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", want.ID, got, want)
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
		"duplicate usb":    {map[string]string{"data/m.yaml": media, "data/a.yaml": model("A", "04f9:0001", "tape-a"), "data/b.yaml": model("B", "04f9:0001", "tape-a")}, []string{"04f9:0001", "A", "B", "a.yaml", "b.yaml"}},
		"unknown media":    {map[string]string{"data/m.yaml": media, "data/a.yaml": model("A", "04f9:0001", "tape-x")}, []string{"A", "tape-x", "a.yaml"}},
		"no dialect":       {map[string]string{"data/a.yaml": "models:\n  - {id: A, driver: brother}\n"}, []string{"A", "dialect"}},
		"second document":  {map[string]string{"data/m.yaml": media, "data/a.yaml": model("A", "04f9:0001", "tape-a") + "---\n" + model("B", "04f9:0002", "tape-a")}, []string{"a.yaml", "one YAML document"}},
		"null model":       {map[string]string{"data/a.yaml": "models: [null]\n"}, []string{"a.yaml", "empty model"}},
		"same status code": {map[string]string{"data/q.yaml": "models:\n  - {id: Q1, driver: brother, dialect: ql, match: {status_code: 0x39}}\n  - {id: Q2, driver: brother, dialect: ql, match: {status_code: 0x39}}\n"}, []string{"Q1", "Q2", "0x39"}},
		"bad snmp pattern": {map[string]string{"data/q.yaml": "models:\n  - {id: Q1, driver: brother, dialect: ql, match: {snmp_descr: \"QL-(\"}}\n"}, []string{"Q1", "snmp_descr"}},
		"unknown tested":   {map[string]string{"data/q.yaml": "models:\n  - {id: Q1, driver: brother, dialect: ql, tested: yes}\n"}, []string{"Q1", "tested"}},
		"note untested":    {map[string]string{"data/q.yaml": "models:\n  - {id: Q1, driver: brother, dialect: ql, tested_note: works}\n"}, []string{"Q1", "tested_note"}},
		"escpos no pins":   {map[string]string{"data/e.yaml": "models:\n  - {id: E1, driver: escpos, dialect: raster}\n"}, []string{"E1", "head_pins"}},
		"media too wide":   {map[string]string{"data/m.yaml": "media:\n  - {id: wide, name: w, kind: continuous, width_mm: 62, printable_width_dots: 696, offset_pins: 32, dpi: 300}\n", "data/q.yaml": "models:\n  - {id: Q1, driver: brother, dialect: ql, head_pins: 720, media: [wide]}\n"}, []string{"Q1", "wide", "720"}},
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

func TestQLUSBMatches(t *testing.T) {
	db := devicedb.Default()
	for id, usb := range map[string]string{"QL-800": "04f9:209b", "QL-810W": "04f9:209c", "QL-820NWB": "04f9:209d"} {
		m, _ := db.Model(id)
		if len(m.Match.USB) != 1 || m.Match.USB[0] != usb || m.Match.IEEE1284Model != id || !slices.Contains(m.Transports, "usb") {
			t.Errorf("%s: %+v transports %v", id, m.Match, m.Transports)
		}
	}
	if m, _ := db.Model("QL-800"); slices.Contains(m.Transports, "tcp") || m.Match.SNMPDescr != "" {
		t.Errorf("QL-800 has no network port: %v %q", m.Transports, m.Match.SNMPDescr)
	}
}

func TestModelByHI(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct{ hi, want string }{
		{"GK420d-200dpi,V61.17.11Z,8,2104KB", "GK420d"},
		{"\x02GK420d-200dpi,V61.17.11Z,8,2104KB\x03", "GK420d"},
		{"ZD420-300dpi,V84.20.18Z,12,8176KB", "ZD420-300"},
		{"ZD420-203dpi,V84.20.18Z,8,8176KB", "ZD420-203"},
		{"LP2844-Z,V45.11.7Z,8,512KB", "LP2844-Z"},
		{"LaserJet 4,V1,8,1KB", ""},
		{"garbage", ""},
	} {
		m, ok := db.ModelByHI(tc.hi)
		if got := ""; ok {
			got = m.ID
			if got != tc.want {
				t.Errorf("%q: %s, want %q", tc.hi, got, tc.want)
			}
		} else if tc.want != "" {
			t.Errorf("%q: not found, want %s", tc.hi, tc.want)
		}
	}
}

// Catalogue sizes are in mm; a model gets them in its own dots.
func TestMediaForFillsDots(t *testing.T) {
	db := devicedb.Default()
	gk, _ := db.Model("GK420d")
	var got *devicedb.Media
	for _, md := range db.MediaFor(gk) {
		if md.ID == "label-102x102" {
			got = &md
		}
	}
	if got == nil || got.DPI != 203 || got.PrintableWidthDots != 816 || got.PrintableLengthDots != 816 || got.Kind != devicedb.DieCut {
		t.Fatalf("label-102x102 on GK420d: %+v", got)
	}
	zd, _ := db.Model("ZD410")
	for _, md := range db.MediaFor(zd) {
		if md.WidthMM > 56 || md.PrintableWidthDots > zd.HeadPins {
			t.Errorf("ZD410 lists %s (%v mm, %d dots)", md.ID, md.WidthMM, md.PrintableWidthDots)
		}
	}
	z300, _ := db.Model("ZD420-300")
	for _, md := range db.MediaFor(z300) {
		if md.ID == "label-102x152" && (md.DPI != 300 || md.PrintableLengthDots != 1824) {
			t.Errorf("label-102x152 at 300 dpi: %+v", md)
		}
	}
}

// A Zebra on USB names itself in the IEEE 1284 MODEL field ("ZTC GK420d", "ZTC ZD420-300dpi ZPL").
func TestModelByZebraMDL(t *testing.T) {
	db := devicedb.Default()
	for mdl, want := range map[string]string{"ZTC GK420d": "GK420d", "ZTC GX430t": "", "ZTC GC420t": "GC420t",
		"ZTC ZD420-300dpi ZPL": "ZD420-300", "ZTC ZD420-203dpi ZPL": "ZD420-203", "QL-810W": "",
		"ZTC ZP 450": "ZP450", " ZTC  GK420d ": "GK420d"} {
		m, ok := db.ModelByZebraMDL(mdl)
		got := ""
		if ok {
			got = m.ID
		}
		if mdl == "ZTC GX430t" { // 300 dpi only
			want = "GX430t"
		}
		if got != want {
			t.Errorf("%q → %q, want %q", mdl, got, want)
		}
	}
}

func TestModelByBLEName(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct{ name, want string }{
		{"MX10", "MX10"}, {"GB03", "GB03"}, {"YT01", "YT01"}, {"MX10-1234", ""}, {"Phone", ""}, {"", ""},
	} {
		got := ""
		if m, ok := db.ModelByBLEName(tc.name); ok {
			got = m.ID
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCatModels(t *testing.T) {
	db := devicedb.Default()
	for _, id := range []string{"GB01", "GB02", "GB03", "GT01", "YT01", "MX05", "MX06", "MX08", "MX10"} {
		m, ok := db.Model(id)
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if m.Driver != "cat" || m.Dialect != "gb" || m.DPI != 203 || m.HeadPins != 384 || !slices.Equal(m.Transports, []string{"ble"}) {
			t.Errorf("%s: %+v", id, m)
		}
		if m.Quirk("ble_service", 0) != 0xae30 || m.Quirk("ble_write", 0) != 0xae01 || m.Quirk("ble_notify", 0) != 0xae02 ||
			m.Quirk("ble_gap_ms", 0) != 10 || m.Quirk("feed_lines", 0) != 80 || !m.QuirkBool("darkness") || !m.QuirkBool("needs_status") {
			t.Errorf("%s quirks: %v", id, m.Quirks)
		}
	}
	mx, _ := db.Model("MX10")
	md := db.MediaFor(mx)
	if len(md) != 1 || md[0].ID != "paper-57" || md[0].PrintableWidthDots != 384 || md[0].DPI != 203 || md[0].Kind != devicedb.Continuous {
		t.Errorf("MX10 media: %+v", md)
	}
}

func TestBadBLEName(t *testing.T) {
	fsys := fstest.MapFS{"data/a.yaml": &fstest.MapFile{Data: []byte("models:\n  - {id: A, driver: cat, dialect: gb, match: {ble_name: \"MX(\"}}\n")}}
	if _, err := devicedb.Load(fsys); err == nil || !strings.Contains(err.Error(), "ble_name") {
		t.Errorf("err = %v", err)
	}
}

func TestModelByBTName(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct{ name, want string }{
		{"RG-MLP80B", "RG-MLP80B"}, {"RG-MLP80B-1234", "RG-MLP80B"}, {"MX10", ""}, {"", ""},
	} {
		got := ""
		if m, ok := db.ModelByBTName(tc.name); ok {
			got = m.ID
		}
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEscposModels(t *testing.T) {
	db := devicedb.Default()
	for id, want := range map[string]struct {
		pins  int
		media string
		dots  int
	}{"RG-MLP80B": {576, "paper-80", 576}, "ESCPOS-80": {576, "paper-80", 576}, "ESCPOS-58": {384, "paper-57", 384}} {
		m, ok := db.Model(id)
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if m.Driver != "escpos" || m.DPI != 203 || m.HeadPins != want.pins || !slices.Equal(m.Transports, []string{"bt"}) ||
			m.Quirk("feed_lines", 0) != 80 || m.Quirk("confirm_timeout_ms", 0) != 20000 {
			t.Errorf("%s: %+v", id, m)
		}
		md := db.MediaFor(m)
		if len(md) != 1 || md[0].ID != want.media || md[0].PrintableWidthDots != want.dots || md[0].Kind != devicedb.Continuous {
			t.Errorf("%s media: %+v", id, md)
		}
	}
}
