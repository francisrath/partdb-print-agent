package pdfgen

import (
	"regexp"
	"strconv"
	"testing"
)

func TestTestCardLinesFit(t *testing.T) {
	long := "v0.0.0-20261027104429-2e18cb8f1a2b+dirty"
	pdf := string(TestCard(29, 90, []string{"pdb-print test", long}))
	m := regexp.MustCompile(`/F1 ([0-9.]+) Tf 3 [0-9.]+ Td \(([^)]*)\)`).FindAllStringSubmatch(pdf, -1)
	if len(m) != 2 {
		t.Fatalf("text lines: %v", m)
	}
	short, _ := strconv.ParseFloat(m[0][1], 64)
	shrunk, _ := strconv.ParseFloat(m[1][1], 64)
	if shrunk >= short {
		t.Errorf("long line not shrunk: %v vs %v", shrunk, short)
	}
	// 0.6 em per glyph is a generous Helvetica estimate; it must fit inside the 29 mm card minus margins
	if w := 0.6 * shrunk * float64(len(long)); w > 29-4.5 {
		t.Errorf("long line ~%.1f mm wide", w)
	}
}
