package brotherql

import "github.com/francisrath/partdb-print-agent/internal/driver"

// Values from Brother's public "Raster Command Reference" for QL-800/810W/820NWB.

type model struct {
	name string
	code byte // status byte 4
}

var models = []model{
	{"QL-800", 0x38},
	{"QL-810W", 0x39},
	{"QL-820NWB", 0x41},
}

const (
	dpi         = 300
	headPins    = 720 // 90 bytes per raster line
	lineBytes   = headPins / 8
	invalidateN = 400
	feedMargin  = 35  // dots (~3 mm) lead/trail on continuous tape
	minLines    = 150 // shortest raster the QL-8xx accepts
)

type mediaSpec struct {
	driver.Media
	rightPins int // unprinted pins before the printable area (spec "right margin")
}

func media(id, name string, kind driver.MediaKind, w, l float64, dotsW, dotsL, right int) mediaSpec {
	return mediaSpec{driver.Media{ID: id, Name: name, Kind: kind, WidthMM: w, LengthMM: l,
		PrintableWidthDots: dotsW, PrintableLengthDots: dotsL, DPI: dpi}, right}
}

var mediaTable = []mediaSpec{
	media("dk-22214", "12 mm continuous", driver.Continuous, 12, 0, 106, 0, 29),
	media("dk-22210", "29 mm continuous", driver.Continuous, 29, 0, 306, 0, 6),
	media("dk-22225", "38 mm continuous", driver.Continuous, 38, 0, 413, 0, 12),
	media("dk-22223", "50 mm continuous", driver.Continuous, 50, 0, 554, 0, 12),
	media("dk-n55224", "54 mm continuous", driver.Continuous, 54, 0, 590, 0, 0),
	media("dk-22205", "62 mm continuous", driver.Continuous, 62, 0, 696, 0, 12),
	media("dk-11204", "17×54 mm die-cut", driver.DieCut, 17, 54, 165, 566, 0),
	media("dk-11201", "29×90 mm die-cut", driver.DieCut, 29, 90, 306, 991, 6),
	media("dk-11208", "38×90 mm die-cut", driver.DieCut, 38, 90, 413, 991, 12),
	media("dk-11209", "62×29 mm die-cut", driver.DieCut, 62, 29, 696, 271, 12),
	media("dk-11202", "62×100 mm die-cut", driver.DieCut, 62, 100, 696, 1109, 12),
}

func lookupMedia(id string) (mediaSpec, bool) {
	for _, m := range mediaTable {
		if m.ID == id {
			return m, true
		}
	}
	return mediaSpec{}, false
}

// mediaBySize maps the width/length the printer reports to a table entry.
func mediaBySize(kind driver.MediaKind, w, l int) (mediaSpec, bool) {
	for _, m := range mediaTable {
		if m.Kind == kind && int(m.WidthMM) == w && int(m.LengthMM) == l {
			return m, true
		}
	}
	return mediaSpec{}, false
}

// LookupMedia returns a known media by id (e.g. "dk-22210").
func LookupMedia(id string) (driver.Media, bool) {
	m, ok := lookupMedia(id)
	return m.Media, ok
}

// Models lists the supported model names.
func Models() []string {
	var out []string
	for _, m := range models {
		out = append(out, m.name)
	}
	return out
}

// MediaFor maps a reported roll (kind and size in mm) to a known media.
func MediaFor(kind driver.MediaKind, widthMM, lengthMM int) (driver.Media, bool) {
	m, ok := mediaBySize(kind, widthMM, lengthMM)
	return m.Media, ok
}
