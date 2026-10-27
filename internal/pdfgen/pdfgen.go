// Package pdfgen writes minimal one-page PDFs (test labels and fixtures).
package pdfgen

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

// Page returns a wMM×hMM PDF. content is a PDF content stream in millimetre
// units (origin bottom-left) and may use Helvetica as /F1.
func Page(wMM, hMM float64, content string) []byte { return Pages(wMM, hMM, content) }

// Pages returns a PDF with one wMM×hMM page per content stream (see Page).
func Pages(wMM, hMM float64, contents ...string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"", // page tree, filled in below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var kids []string
	for _, c := range contents {
		c = "2.8346 0 0 2.8346 0 0 cm " + c
		n := len(objs) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", n))
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", wMM/25.4*72, hMM/25.4*72, n+1),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(c), c))
	}
	objs[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(contents))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// Text escapes s for a PDF literal string (ASCII only; other runes become '?').
func Text(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestCard is a printer test label of wMM×hMM: border, 1 mm rulers on the
// top and left edges, a 0.5 mm checkerboard and the given text lines.
func TestCard(wMM, hMM float64, lines []string) []byte {
	var c strings.Builder
	fmt.Fprintf(&c, "0 g 0.3 w 0.5 0.5 %.2f %.2f re S\n0.15 w\n", wMM-1, hMM-1)
	for x := 1.0; x < wMM-0.5; x++ { // top ruler, longer tick every 5 mm
		l := 1.0
		if int(x)%5 == 0 {
			l = 2
		}
		fmt.Fprintf(&c, "%.2f %.2f m %.2f %.2f l S\n", x, hMM-0.5, x, hMM-0.5-l)
	}
	for y := 1.0; y < hMM-0.5; y++ { // left ruler
		l := 1.0
		if int(y)%5 == 0 {
			l = 2
		}
		fmt.Fprintf(&c, "0.5 %.2f m %.2f %.2f l S\n", y, 0.5+l, y)
	}
	size := min(max(wMM/10, 1.2), 3.2)
	y := hMM - 3 - size
	for _, s := range lines {
		// shrink long lines to fit the width; rounded down because Tf is written with 2 decimals
		fs := math.Floor(min(size, (wMM-4.5)/max(textWidthEm(s), 0.001))*100) / 100
		fmt.Fprintf(&c, "BT /F1 %.2f Tf 3 %.2f Td (%s) Tj ET\n", fs, y, Text(s))
		y -= size * 1.3
	}
	n := int(min(wMM-6, y-3) / 0.5) // checkerboard fills what's left, square
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if (i+j)%2 == 0 {
				fmt.Fprintf(&c, "%.2f %.2f 0.5 0.5 re f\n", 3+float64(i)*0.5, 3+float64(j)*0.5)
			}
		}
	}
	return Page(wMM, hMM, c.String())
}

// helveticaWidths are the advance widths (1/1000 em) of ASCII 32–126 in Helvetica,
// from Adobe's Core 14 font metrics (Helvetica.afm).
var helveticaWidths = [95]int{
	278, 278, 355, 556, 556, 889, 667, 222, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	222, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
}

// textWidthEm is the width of s in Helvetica, in em. Text turns anything outside ASCII into '?'.
func textWidthEm(s string) float64 {
	w := 0
	for _, r := range s {
		if r < 32 || r > 126 {
			r = '?'
		}
		w += helveticaWidths[r-32]
	}
	return float64(w) / 1000
}
