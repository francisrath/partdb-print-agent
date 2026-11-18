package brotherpt

import (
	"testing"

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

func TestParseStatus(t *testing.T) {
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
			s, typ, err := parseStatus(tc.in)
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
	if s, _, _ := parseStatus(reply(func(b []byte) { b[10] = 5 })); s.Media.WidthMM != 5 {
		t.Errorf("unknown media width %v, want 5", s.Media.WidthMM)
	}
}

func TestParseStatusRejectsGarbage(t *testing.T) {
	for name, in := range map[string][]byte{
		"bad header": reply(func(b []byte) { b[0] = 0x81 }),
		"short":      realReply()[:31],
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseStatus(in)
			if e, ok := err.(*driver.Error); !ok || e.Code != driver.CommsError {
				t.Fatalf("err = %v, want COMMS_ERROR", err)
			}
		})
	}
}

func TestMediaTable(t *testing.T) {
	for _, tc := range []struct {
		id         string
		dots, offs int
	}{
		{"tze-6", 64, 160}, {"tze-9", 106, 139}, {"tze-12", 150, 117},
		{"tze-18", 234, 75}, {"tze-24", 320, 32}, {"tze-36", 384, 0},
	} {
		m, ok := lookupMedia(tc.id)
		if !ok || m.PrintableWidthDots != tc.dots || m.offsetPins != tc.offs || m.DPI != 360 || m.Kind != driver.Continuous {
			t.Errorf("%s: %+v", tc.id, m)
		}
		if w, ok := MediaForWidth(int(m.WidthMM)); !ok || w.ID != tc.id {
			t.Errorf("MediaForWidth(%v) = %v", m.WidthMM, w.ID)
		}
	}
	if _, ok := LookupMedia("dk-22210"); ok {
		t.Error("QL media accepted")
	}
}

func mediaID(m *driver.Media) string {
	if m == nil {
		return ""
	}
	return m.ID
}
