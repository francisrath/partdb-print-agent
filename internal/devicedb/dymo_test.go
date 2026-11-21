package devicedb_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
)

func TestDymoModels(t *testing.T) {
	db := devicedb.Default()
	models := db.Models("dymo")
	if len(models) < 11 {
		t.Fatalf("dymo models: %d", len(models))
	}
	for _, m := range models {
		head := 672
		if m.ID == "LabelWriter-4XL" {
			head = 1248 // 4.16 in at 300 dpi
		}
		if m.Vendor != "DYMO" || m.DPI != 300 || m.HeadPins != head || !slices.Equal(m.Transports, []string{"usb"}) ||
			(m.Dialect != "lw400" && m.Dialect != "lw300") || m.QuirkBool("darkness") != (m.Dialect == "lw400") {
			t.Errorf("%s: %+v", m.ID, m)
		}
		if len(m.Match.USB) == 0 {
			t.Errorf("%s: no USB id", m.ID)
		}
		for _, u := range m.Match.USB {
			if !strings.HasPrefix(u, "0922:") {
				t.Errorf("%s: usb %s isn't DYMO's vendor 0922", m.ID, u)
			}
		}
		for _, md := range db.MediaFor(m) {
			if !strings.HasPrefix(md.ID, "dymo-") || md.DPI != 300 || md.PrintableWidthDots > m.HeadPins || md.OffsetPins != 0 {
				t.Errorf("%s: media %+v", m.ID, md)
			}
			// dots are true 300 dpi, within a dot of the nominal size (US sizes are given in mm to 0.1)
			if d := float64(md.PrintableWidthDots) - md.WidthMM*300/25.4; d > 1.5 || (d < -1.5 && md.PrintableWidthDots != m.HeadPins && md.PrintableWidthDots != 672) {
				t.Errorf("%s: width %v mm = %d dots", md.ID, md.WidthMM, md.PrintableWidthDots)
			}
			if d := float64(md.PrintableLengthDots) - md.LengthMM*300/25.4; md.Kind == devicedb.DieCut && (d > 1.5 || d < -1.5) {
				t.Errorf("%s: length %v mm = %d dots", md.ID, md.LengthMM, md.PrintableLengthDots)
			}
		}
	}
	if _, ok := db.Model("LabelWriter-550"); ok {
		t.Error("the chip-locked 5xx series must not be listed")
	}
	if m, _ := db.Model("LabelWriter-450"); len(db.MediaFor(m)) == 0 || slices.Contains(m.Media, "dymo-1744907") {
		t.Errorf("LabelWriter-450 media: %v", m.Media)
	}
}
