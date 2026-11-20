package devicedb

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Size tolerances for matching a label page to media: a page side within WidthToleranceMM of
// the media width goes across the head; a die-cut label's other side must be within LengthToleranceMM.
const (
	WidthToleranceMM  = 0.6
	LengthToleranceMM = 1.0
)

// Fits reports whether a w×h mm label page prints on m, turned if needed: continuous media
// needs one side to match the width (length free), die-cut media both sides.
func (m *Media) Fits(wMM, hMM float64) bool {
	near := func(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
	if m.Kind != DieCut || m.LengthMM == 0 {
		return near(wMM, m.WidthMM, WidthToleranceMM) || near(hMM, m.WidthMM, WidthToleranceMM)
	}
	return near(wMM, m.WidthMM, WidthToleranceMM) && near(hMM, m.LengthMM, LengthToleranceMM) ||
		near(hMM, m.WidthMM, WidthToleranceMM) && near(wMM, m.LengthMM, LengthToleranceMM)
}

// Describe names media by its size: "12 mm tape" or "17×54 mm labels".
func (m *Media) Describe() string {
	if m.Kind == DieCut && m.LengthMM > 0 {
		return fmt.Sprintf("%g×%g mm labels", m.WidthMM, m.LengthMM)
	}
	return fmt.Sprintf("%g mm tape", m.WidthMM)
}

// Mismatch explains why a w×h mm label doesn't fit loaded, naming what it needs from
// candidates (the media the printer takes):
// "label is 29×46 mm (needs 29 mm tape), printer has dk-22214 (12 mm tape) loaded".
func Mismatch(wMM, hMM float64, loaded *Media, candidates []Media) string {
	var needs []string
	for i := range candidates {
		if d := candidates[i].Describe(); candidates[i].Fits(wMM, hMM) && !slices.Contains(needs, d) {
			needs = append(needs, d)
		}
	}
	need := "no roll for this printer fits it"
	if len(needs) > 0 {
		need = "needs " + strings.Join(needs, " or ")
	}
	r := func(v float64) float64 { return math.Round(v*10) / 10 }
	return fmt.Sprintf("label is %g×%g mm (%s), printer has %s (%s) loaded", r(wMM), r(hMM), need, loaded.ID, loaded.Describe())
}
