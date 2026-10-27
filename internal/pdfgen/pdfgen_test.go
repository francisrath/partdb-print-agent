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
	if w := shrunk * textWidthEm(long); w > 29-4.5 {
		t.Errorf("long line %.1f mm wide", w)
	}
}

func TestTextWidthEm(t *testing.T) {
	// values from Helvetica.afm: W 944, i 222, space 278
	if got := textWidthEm("Wi "); got != 1.444 {
		t.Errorf("textWidthEm = %v", got)
	}
	if textWidthEm("é") != textWidthEm("?") {
		t.Error("non-ASCII should measure as '?'")
	}
}
