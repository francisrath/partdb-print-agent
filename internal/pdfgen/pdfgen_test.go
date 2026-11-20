package pdfgen

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/boombuler/barcode/qr"
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
	if w := shrunk * textWidthEm(long); w > 29-5.5 {
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

// The test card marks its top edge so an upside-down print is obvious.
func TestTestCardMarksTop(t *testing.T) {
	pdf := string(TestCard(29, 40, []string{"pdb-print test"}))
	top := regexp.MustCompile(`Td \(TOP\) Tj`).FindStringIndex(pdf)
	first := regexp.MustCompile(`Td \(pdb-print test\) Tj`).FindStringIndex(pdf)
	if top == nil || first == nil || top[0] > first[0] {
		t.Fatalf("TOP not drawn above the text lines:\n%s", pdf)
	}
	if !regexp.MustCompile(`m [0-9.]+ [0-9.]+ l [0-9.]+ [0-9.]+ l h f`).MatchString(pdf) {
		t.Error("no filled arrow next to TOP")
	}
}

var rectRe = regexp.MustCompile(`([0-9.]+) ([0-9.]+) ([0-9.]+) ([0-9.]+) re f`)

// section returns the content between "% name" and the next "% end".
func section(t *testing.T, pdf, name string) string {
	t.Helper()
	i := strings.Index(pdf, "% "+name+"\n")
	if i < 0 {
		t.Fatalf("no %s section", name)
	}
	return pdf[i : i+strings.Index(pdf[i:], "% end\n")]
}

func TestPartLabelFits(t *testing.T) {
	for _, sz := range [][2]float64{{102, 102}, {102, 152}, {57, 32}, {102, 51}} {
		w, h := sz[0], sz[1]
		pdf := string(PartLabel(w, h, "AMS1117-3.3 LDO regulator", "P-000032", "https://partdb.example.org/en/part/32", "P-000032", "2/5"))
		for _, m := range rectRe.FindAllStringSubmatch(pdf, -1) {
			x, _ := strconv.ParseFloat(m[1], 64)
			y, _ := strconv.ParseFloat(m[2], 64)
			rw, _ := strconv.ParseFloat(m[3], 64)
			rh, _ := strconv.ParseFloat(m[4], 64)
			if x < 0 || y < 0 || x+rw > w+0.001 || y+rh > h+0.001 {
				t.Errorf("%vx%v: rect %v outside the label", w, h, m[0])
			}
		}
		// every dark QR module is drawn, nothing else: the drawn area is modules × module²
		code, _ := qr.Encode("https://partdb.example.org/en/part/32", qr.M, qr.Auto)
		dark := 0
		b := code.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if r, _, _, _ := code.At(x, y).RGBA(); r == 0 {
					dark++
				}
			}
		}
		var area, mod float64
		for i, m := range rectRe.FindAllStringSubmatch(section(t, pdf, "qr"), -1) {
			rw, _ := strconv.ParseFloat(m[3], 64)
			rh, _ := strconv.ParseFloat(m[4], 64)
			if i == 0 {
				mod = rh
			}
			area += rw * rh
		}
		if mod < 0.3 || math.Abs(area-float64(dark)*mod*mod) > 0.01*area {
			t.Errorf("%vx%v: QR area %.2f mm², want %d modules of %.3f mm", w, h, area, dark, mod)
		}
		if bars := rectRe.FindAllString(section(t, pdf, "code128"), -1); len(bars) < 10 {
			t.Errorf("%vx%v: %d Code 128 bars", w, h, len(bars))
		}
		if !strings.Contains(pdf, "(2/5) Tj") {
			t.Errorf("%vx%v: no tag", w, h)
		}
	}
}

func TestTestCardTag(t *testing.T) {
	if pdf := string(TestCardTagged(102, 102, []string{"pdb-print test"}, "1/5")); !strings.Contains(pdf, "(1/5) Tj") {
		t.Error("no tag on the test card")
	}
}

func TestTestCardsTagged(t *testing.T) {
	pdf := string(TestCardsTagged(102, 102, []string{"batch"}, "3a/5", "3b/5"))
	if strings.Count(pdf, "/Type /Page ") != 2 || !strings.Contains(pdf, "(3a/5) Tj") || !strings.Contains(pdf, "(3b/5) Tj") {
		t.Errorf("want two pages tagged 3a and 3b")
	}
}

// The border sits 1 mm inside the card, so a label a little smaller than its nominal size
// (a 4" roll is 101.6 mm, the card 102 mm) or ±1 mm of feed tolerance doesn't cut it off.
func TestTestCardBorderInset(t *testing.T) {
	pdf := string(TestCard(102, 102, []string{"pdb-print test"}))
	if !strings.Contains(pdf, "1.00 1.00 100.00 100.00 re S") {
		t.Errorf("border not 1 mm inside:\n%.300s", pdf[strings.Index(pdf, "re S")-60:])
	}
}
