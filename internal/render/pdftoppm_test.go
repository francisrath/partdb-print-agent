package render

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
)

func TestParseInfo(t *testing.T) {
	out := `Producer:        dompdf
Pages:           3
Encrypted:       no
Page    1 size:  82.205 x 130.394 pts
Page    1 rot:   0
Page    2 size:  595.276 x 841.89 pts (A4)
Page    2 rot:   90
Page    3 size:  141.732 x 82.205 pts
Page    3 rot:   270
File size:       2402 bytes
`
	got, err := parseInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []pageSize{{82.205, 130.394, false}, {841.89, 595.276, true}, {82.205, 141.732, true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{
		"",
		"Pages: 2\nPage    1 size:  10 x 10 pts\n", // a page missing
		"Pages: 1\nPage    1 size:  0 x 10 pts\n",  // empty page
		"Pages: 1\nPage    2 size:  10 x 10 pts\n", // out of range
	} {
		if _, err := parseInfo(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func pgmBytes(w, h int, pix ...byte) []byte {
	return append([]byte(fmt.Sprintf("P5\n%d %d\n255\n", w, h)), pix...)
}

func TestParsePGMs(t *testing.T) {
	two := append(pgmBytes(2, 1, 0, 255), pgmBytes(2, 1, 9, 8)...)
	imgs, err := parsePGMs(two, 2, 2, 1)
	if err != nil || len(imgs) != 2 || imgs[1].Pix[0] != 9 {
		t.Fatalf("%v %v", imgs, err)
	}
	for name, tc := range map[string]struct {
		b       []byte
		n, w, h int
	}{
		"wrong size":   {pgmBytes(2, 1, 0, 0), 1, 3, 1},
		"short":        {pgmBytes(2, 1, 0), 1, 2, 1},
		"trailing":     {append(pgmBytes(2, 1, 0, 0), 7), 1, 2, 1},
		"missing page": {pgmBytes(2, 1, 0, 0), 2, 2, 1},
		"16 bit":       {[]byte("P5\n1 1\n65535\n\x00\x00"), 1, 1, 1},
		"PBM":          {[]byte("P4\n1 1\n\x00"), 1, 1, 1},
	} {
		if _, err := parsePGMs(tc.b, tc.n, tc.w, tc.h); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOutputWriters(t *testing.T) {
	tl := &tail{n: 4}
	for _, s := range []string{"ab", "cdef", "g"} {
		if n, err := tl.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatal(n, err)
		}
	}
	if string(tl.b) != "defg" {
		t.Errorf("tail %q", tl.b)
	}
	c := &capped{max: 5}
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("def")); err == nil || c.String() != "abc" {
		t.Errorf("over the cap: %v, kept %q", err, c.String())
	}
}

func TestRotateCW(t *testing.T) {
	// 3×2:  a b c
	//       d e f
	img := image.NewGray(image.Rect(0, 0, 3, 2))
	copy(img.Pix, "abcdef")
	for q, want := range []string{"abcdef", "daebfc", "fedcba", "cfbead"} {
		got := rotateCW(img, q)
		if string(got.Pix) != want {
			t.Errorf("%d quarter turns: %q, want %q", q, got.Pix, want)
		}
	}
}

func TestPdftoppmMissing(t *testing.T) {
	p := &Pdftoppm{Bin: "/nonexistent/pdftoppm", Info: "/nonexistent/pdfinfo"}
	_, err := p.Render(testPDF(29, 46, "x"), Options{WidthDots: 306, MediaWidthMM: 29})
	if !errors.Is(err, ErrPdftoppm) {
		t.Errorf("want ErrPdftoppm, got %v", err)
	}
	for _, interp := range []bool{false, true} {
		if p, err := findPdftoppm("off", interp); p != nil || err != nil {
			t.Errorf("off: %v %v", p, err)
		}
	}
	if p, err := findPdftoppm("auto", false); p != nil || err != nil {
		t.Errorf("auto with a compiling wazero: %v %v", p, err)
	}
	if _, err := FindPdftoppm("/nonexistent/pdftoppm"); err == nil {
		t.Error("missing explicit path accepted")
	}
}

// withRotate sets /Rotate on every page (the xref goes stale; both renderers repair it).
func withRotate(pdf []byte, deg int) []byte {
	return bytes.ReplaceAll(pdf, []byte("/Type /Page /Parent"), []byte(fmt.Sprintf("/Type /Page /Rotate %d /Parent", deg)))
}

// pdftoppm must give the same sizes, rotations and errors as PDFium, and nearly the
// same pixels (the rasterisers round edges differently, by at most a dot).
func TestPdftoppmMatchesPDFium(t *testing.T) {
	pp, err := FindPdftoppm("on")
	if err != nil || pp == nil {
		t.Skip("pdftoppm not installed")
	}
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tests := []struct {
		name string
		pdf  []byte
		opt  Options
	}{
		{"29x46 portrait", testPDF(29, 46, "AMS1117-3.3"), Options{WidthDots: 306, MediaWidthMM: 29}},
		{"50x29 auto-rotated", testPDF(50, 29, "AMS1117-3.3"), Options{WidthDots: 306, MediaWidthMM: 29}},
		{"12x16", testPDF(12, 16, "AMS1117-3.3"), Options{WidthDots: 106, MediaWidthMM: 12}},
		{"54x17 die-cut", testPDF(54, 17, "AMS1117-3.3"), Options{WidthDots: 165, LengthDots: 566, MediaWidthMM: 17}},
		{"rotate 90", testPDF(29, 46, "AMS1117-3.3"), Options{WidthDots: 306, MediaWidthMM: 29, Rotate: "90"}},
		{"rotate 180", testPDF(29, 46, "AMS1117-3.3"), Options{WidthDots: 306, MediaWidthMM: 29, Rotate: "180"}},
		{"/Rotate 90 in the PDF", withRotate(testPDF(46, 29, "AMS1117-3.3"), 90), Options{WidthDots: 306, MediaWidthMM: 29}},
		{"two pages", pdfgen.Pages(29, 46, "0 g 2 2 10 10 re f", "0 g 2 2 10 10 re f"), Options{WidthDots: 306, MediaWidthMM: 29}},
		{"threshold", testPDF(29, 46, "x"), Options{WidthDots: 306, MediaWidthMM: 29, Threshold: 200}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want, err := r.Render(tc.pdf, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			got, err := pp.Render(tc.pdf, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%d pages, PDFium %d", len(got), len(want))
			}
			for i := range got {
				if got[i].Bounds() != want[i].Bounds() {
					t.Fatalf("page %d: %v, PDFium %v", i+1, got[i].Bounds(), want[i].Bounds())
				}
				for _, v := range got[i].Pix {
					if v != 0 && v != 255 {
						t.Fatalf("non-binary pixel %d", v)
					}
				}
				n := len(got[i].Pix)
				if d := countDiff(got[i], want[i]); d*100 > n*3 {
					t.Errorf("page %d: %d of %d pixels differ from PDFium", i+1, d, n)
				}
			}
		})
	}

	// A different-size page in the middle gets its own pdftoppm run.
	mixed := []byte(strings.Replace(string(pdfgen.Pages(29, 46, "0 g 1 1 5 5 re f", "0 g 1 1 5 5 re f", "0 g 1 1 5 5 re f")),
		"/MediaBox [0 0 82.205 130.394] /Contents 7", "/MediaBox [0 0 82.205 200.000] /Contents 7", 1))
	pages, err := pp.Render(mixed, Options{WidthDots: 306, MediaWidthMM: 29})
	if err != nil || len(pages) != 3 {
		t.Fatalf("mixed sizes: %d pages, %v", len(pages), err)
	}
	if h := []int{pages[0].Bounds().Dy(), pages[1].Bounds().Dy(), pages[2].Bounds().Dy()}; h[0] != 485 || h[1] != 744 || h[2] != 485 {
		t.Errorf("mixed sizes: heights %v", h)
	}

	_, err = pp.Render(testPDF(62, 100, "x"), Options{WidthDots: 306, MediaWidthMM: 29, CheckSize: true})
	if !errors.Is(err, ErrSize) {
		t.Errorf("size check: %v", err)
	}
	small := *pp
	small.maxPixels = 306 * 400
	if _, err = small.Render(testPDF(29, 46, "x"), Options{WidthDots: 306, MediaWidthMM: 29}); err == nil ||
		errors.Is(err, ErrPdftoppm) || !strings.Contains(err.Error(), "too large") {
		t.Errorf("pixel budget: %v", err)
	}
	if _, err = pp.Render([]byte("%PDF-1.4 garbage"), Options{WidthDots: 306}); !errors.Is(err, ErrPdftoppm) {
		t.Errorf("broken PDF: %v", err)
	}
}
