package brotherpt

import (
	"strconv"

	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// Values from Brother's "PT-9500PC Command Reference, CBP-RASTER Mode (PTCBP Mode) Volume",
// v1.0, 2003-08-29 (page numbers in comments are that document's).

const (
	model       = "PT-9500PC"
	dpi         = 360
	headPins    = 384   // 48 bytes per full raster line (p.11)
	minLines    = 57    // 4 mm minimum label length at 360 dpi (p.9)
	maxLines    = 14173 // 1000 mm maximum (p.9)
	margin      = 28    // feed before/after the label in dots (~2 mm; allowed 14–1800, p.9)
	invalidateN = 100
)

type mediaSpec struct {
	driver.Media
	offsetPins int // unprinted pins from pin 0 to the print area (p.11)
}

func tape(mm, tapeDots, dots, offset int) mediaSpec {
	return mediaSpec{driver.Media{
		ID: "tze-" + strconv.Itoa(mm), Name: strconv.Itoa(mm) + " mm tape", Kind: driver.Continuous, WidthMM: float64(mm),
		PrintableWidthDots: dots, WidthDots: tapeDots, DPI: dpi,
	}, offset}
}

// Tape width and print area in dots (p.6) and the pins the Windows driver prints them on (p.11).
var mediaTable = []mediaSpec{
	tape(6, 84, 64, 160),
	tape(9, 128, 106, 139),
	tape(12, 170, 150, 117),
	tape(18, 256, 234, 75),
	tape(24, 340, 320, 32),
	tape(36, 512, 384, 0),
}

func lookupMedia(id string) (mediaSpec, bool) {
	for _, m := range mediaTable {
		if m.ID == id {
			return m, true
		}
	}
	return mediaSpec{}, false
}

// LookupMedia returns a known tape by id (e.g. "tze-24").
func LookupMedia(id string) (driver.Media, bool) {
	m, ok := lookupMedia(id)
	return m.Media, ok
}

// MediaForWidth maps the tape width the printer reports to a known tape.
func MediaForWidth(mm int) (driver.Media, bool) {
	for _, m := range mediaTable {
		if int(m.WidthMM) == mm {
			return m.Media, true
		}
	}
	return driver.Media{}, false
}

// Models lists the supported model names.
func Models() []string { return []string{model} }
