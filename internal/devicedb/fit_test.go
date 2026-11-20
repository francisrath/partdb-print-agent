package devicedb

import "testing"

func TestFits(t *testing.T) {
	db := Default()
	tests := []struct {
		media    string
		wMM, hMM float64
		ok       bool
	}{
		{"dk-22214", 12, 16, true},     // lid label, portrait
		{"dk-22214", 16, 12, true},     // the same label, landscape PDF
		{"dk-22214", 29, 46, false},    // bag label on 12 mm
		{"dk-22210", 29, 46, true},     // bag label, length free
		{"dk-22210", 50, 29, true},     // landscape PDF, turned
		{"dk-22210", 29.5, 300, true},  // within the width tolerance
		{"dk-22210", 29.7, 46, false},  // just outside it
		{"dk-22205", 62, 100, true},    // 62 mm
		{"dk-22205", 29, 46, false},    // 29 mm label on 62 mm tape
		{"dk-11204", 17, 54, true},     // die-cut 17×54
		{"dk-11204", 54, 17, true},     // rotated PDF
		{"dk-11204", 17.4, 54.9, true}, // tolerance on both sides
		{"dk-11204", 17, 40, false},    // too short for the die-cut label
		{"dk-11204", 29, 90, false},    // other die-cut size
		{"dk-11201", 29, 90, true},     // die-cut 29×90
		{"dk-11201", 90, 29, true},     // rotated
		{"dk-11201", 29, 46, false},    // continuous-length label on die-cut
	}
	for _, tc := range tests {
		m, ok := db.Media(tc.media)
		if !ok {
			t.Fatalf("no media %s", tc.media)
		}
		if got := m.Fits(tc.wMM, tc.hMM); got != tc.ok {
			t.Errorf("%gx%g on %s: fits=%v, want %v", tc.wMM, tc.hMM, tc.media, got, tc.ok)
		}
	}
}

func TestMismatch(t *testing.T) {
	db := Default()
	ql, _ := db.Model("QL-810W")
	cands := db.MediaFor(ql)
	media := func(id string) *Media { m, _ := db.Media(id); return &m }
	tests := []struct {
		wMM, hMM float64
		loaded   string
		want     string
	}{
		{29, 46, "dk-22214", "label is 29×46 mm (needs 29 mm tape), printer has dk-22214 (12 mm tape) loaded"},
		{12, 16, "dk-22210", "label is 12×16 mm (needs 12 mm tape), printer has dk-22210 (29 mm tape) loaded"},
		{54.01, 17, "dk-22214", "label is 54×17 mm (needs 54 mm tape or 17×54 mm labels), printer has dk-22214 (12 mm tape) loaded"},
		{70, 70, "dk-11204", "label is 70×70 mm (no roll for this printer fits it), printer has dk-11204 (17×54 mm labels) loaded"},
	}
	for _, tc := range tests {
		if got := Mismatch(tc.wMM, tc.hMM, media(tc.loaded), cands); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}
