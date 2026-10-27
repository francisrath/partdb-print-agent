package render

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/klippa-app/go-pdfium/enums"
)

var update = flag.Bool("update", false, "rewrite golden PNGs")

// testPDF builds a one-page PDF of wMM×hMM with a border, a 21×21 QR-like grid
// of 1 mm modules and Helvetica text.
func testPDF(wMM, hMM float64, text string) []byte {
	var c strings.Builder
	fmt.Fprintf(&c, "0 g 0.3 w 0.5 0.5 %.2f %.2f re S\n", wMM-1, hMM-1)
	for y := 0; y < 21; y++ {
		for x := 0; x < 21; x++ {
			if (x*7+y*13+x*y)%3 == 0 || (x < 7 && y < 7 && (x == 0 || y == 0 || x == 6 || y == 6 || (x > 1 && x < 5 && y > 1 && y < 5))) {
				fmt.Fprintf(&c, "%d %d 1 1 re f\n", 2+x, int(hMM)-23+y)
			}
		}
	}
	fmt.Fprintf(&c, "BT /F1 3 Tf 2 2 Td (%s) Tj ET\n", text)
	return pdfgen.Page(wMM, hMM, c.String())
}

func TestRenderGolden(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tests := []struct {
		name       string
		wMM, hMM   float64
		opt        Options
		wantW, wnH int
	}{
		// Part-DB profile 2: 29×46 portrait, content already rotated by CSS → no rotation.
		{"29x46_portrait_29mm", 29, 46, Options{WidthDots: 306, MediaWidthMM: 29}, 306, 485},
		// Plain landscape label: auto-rotated so the 29 mm side goes across the head.
		{"50x29_landscape_29mm", 50, 29, Options{WidthDots: 306, MediaWidthMM: 29}, 306, 528},
		// Part-DB profile 1: 12×16 lid label.
		{"12x16_portrait_12mm", 12, 16, Options{WidthDots: 106, MediaWidthMM: 12}, 106, 141},
		// Die-cut: fill the printable area exactly (like the qlprint reference).
		{"54x17_diecut", 54, 17, Options{WidthDots: 165, LengthDots: 566, MediaWidthMM: 17}, 165, 566},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pages, err := r.Render(testPDF(tc.wMM, tc.hMM, "AMS1117-3.3"), tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 1 {
				t.Fatalf("%d pages", len(pages))
			}
			img := pages[0]
			if img.Bounds().Dx() != tc.wantW || img.Bounds().Dy() != tc.wnH {
				t.Fatalf("size %v, want %dx%d", img.Bounds().Size(), tc.wantW, tc.wnH)
			}
			for _, v := range img.Pix {
				if v != 0 && v != 255 {
					t.Fatalf("non-binary pixel %d", v)
				}
			}
			golden := filepath.Join("testdata", tc.name+".png")
			if *update {
				var b bytes.Buffer
				if err := png.Encode(&b, img); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, b.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f, err := os.Open(golden)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			want, err := png.Decode(f)
			if err != nil {
				t.Fatal(err)
			}
			if diff := countDiff(img, want); diff != 0 {
				t.Errorf("%d pixels differ from %s (inspect, then -update)", diff, golden)
			}
		})
	}
}

func countDiff(a *image.Gray, b image.Image) int {
	if a.Bounds() != b.Bounds() {
		return a.Bounds().Dx() * a.Bounds().Dy()
	}
	n := 0
	for y := 0; y < a.Bounds().Dy(); y++ {
		for x := 0; x < a.Bounds().Dx(); x++ {
			if g, _, _, _ := b.At(x, y).RGBA(); (g > 0x7fff) != (a.GrayAt(x, y).Y > 127) {
				n++
			}
		}
	}
	return n
}

func TestRotation(t *testing.T) {
	cw0, ccw90 := enums.FPDF_PAGE_ROTATION_NONE, enums.FPDF_PAGE_ROTATION_270_CW
	tests := []struct {
		rot          string
		w, h, medium float64
		want         enums.FPDF_PAGE_ROTATION
	}{
		{"auto", 29, 46, 29, cw0},
		{"auto", 46, 29, 29, ccw90},
		{"", 29.3, 90, 29, cw0},
		{"auto", 29, 29, 29, cw0},    // square: both match, leave it
		{"auto", 50, 30, 62, ccw90},  // no match: long side along tape
		{"auto", 62, 100, 62, cw0},   // width matches although it's the short side
		{"auto", 100, 62, 62, ccw90}, // height matches
		{"90", 29, 46, 29, enums.FPDF_PAGE_ROTATION_90_CW},
	}
	for _, tc := range tests {
		got, err := rotation(tc.rot, tc.w, tc.h, tc.medium)
		if err != nil || got != tc.want {
			t.Errorf("rotation(%q, %v×%v on %v) = %v, %v; want %v", tc.rot, tc.w, tc.h, tc.medium, got, err, tc.want)
		}
	}
	if _, err := rotation("45", 1, 1, 1); err == nil {
		t.Error("45° accepted")
	}
}

func TestCheckSize(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tests := []struct {
		name          string
		wMM, hMM      float64
		mediaW, media float64 // media width, die-cut length (0 = continuous)
		ok            bool
	}{
		{"29x46 on 29 mm", 29, 46, 29, 0, true},
		{"50x29 rotated onto 29 mm", 50, 29, 29, 0, true},
		{"12x16 lid label on 29 mm", 12, 16, 29, 0, false},
		{"62 mm label on 29 mm", 62, 100, 29, 0, false},
		{"54x17 on 17x54 die-cut", 54, 17, 17, 54, true},
		{"29x90 label on 17x54 die-cut", 29, 90, 17, 54, false},
		{"17x40 on 17x54 die-cut (too short)", 17, 40, 17, 54, false},
	}
	for _, tc := range tests {
		_, err := r.Render(testPDF(tc.wMM, tc.hMM, "x"), Options{WidthDots: 165, LengthDots: 100,
			MediaWidthMM: tc.mediaW, MediaLengthMM: tc.media, CheckSize: true})
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrSize)) {
			t.Errorf("%s: %v", tc.name, err)
		}

	}
}

func TestPixelBudget(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.maxPixels = 306 * 400 // less than one 29x46 page (306x485)
	_, err = r.Render(testPDF(29, 46, "x"), Options{WidthDots: 306, MediaWidthMM: 29})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("over budget: %v", err)
	}
	// each page fits, the total doesn't: the budget is per PDF, not per page
	r.maxPixels = 306 * 485 * 2
	three := pdfgen.Pages(29, 46, "0 g 1 1 5 5 re f", "0 g 1 1 5 5 re f", "0 g 1 1 5 5 re f")
	if _, err := r.Render(three, Options{WidthDots: 306, MediaWidthMM: 29}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("multi-page over budget: %v", err)
	}
	r.maxPixels = MaxPixels
	if _, err := r.Render(testPDF(29, 46, "x"), Options{WidthDots: 306, MediaWidthMM: 29}); err != nil {
		t.Errorf("within budget: %v", err)
	}
	pages, err := r.Render(three, Options{WidthDots: 306, MediaWidthMM: 29})
	if err != nil || len(pages) != 3 {
		t.Errorf("three pages within budget: %d, %v", len(pages), err)
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.pdf")
	if err := os.WriteFile(small, testPDF(29, 46, "x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(small); err != nil || !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Errorf("small: %v", err)
	}
	big := filepath.Join(dir, "big.pdf")
	if err := os.WriteFile(big, make([]byte, MaxPDFBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(big); err == nil {
		t.Error("oversized file accepted")
	}
}

// The test card's text must stay inside the label even for the widest glyphs: render it
// and check the strip just inside the right border stays white.
func TestTestCardTextFitsRendered(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	card := pdfgen.TestCard(29, 90, []string{"WWWWWWWWWWWWWWWWWWWW", "MMMMMMMMMMMMMMMMMMMMMMMMM"})
	pages, err := r.Render(card, Options{WidthDots: 306, MediaWidthMM: 29})
	if err != nil {
		t.Fatal(err)
	}
	img := pages[0]
	k := 306.0 / 29 // dots per mm
	// columns 27.4–28.2 mm: right of any text margin, left of the border line at 28.5 mm;
	// rows below the top ruler and above the bottom border
	for y := int(3.5 * k); y < img.Bounds().Dy()-int(0.8*k); y++ {
		for x := int(27.4 * k); x <= int(28.2*k); x++ {
			if img.GrayAt(x, y).Y < 128 {
				t.Fatalf("text reaches the border at (%d,%d)", x, y)
			}
		}
	}
}
