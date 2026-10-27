package render

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klippa-app/go-pdfium/enums"
)

var update = flag.Bool("update", false, "rewrite golden PNGs")

// testPDF builds a one-page PDF of wMM×hMM with a border, a 21×21 QR-like grid
// of 1 mm modules and Helvetica text. Content stream units are mm.
func testPDF(wMM, hMM float64, text string) []byte {
	var c strings.Builder
	fmt.Fprintf(&c, "2.8346 0 0 2.8346 0 0 cm 0 g 0.3 w 0.5 0.5 %.2f %.2f re S\n", wMM-1, hMM-1)
	for y := 0; y < 21; y++ {
		for x := 0; x < 21; x++ {
			if (x*7+y*13+x*y)%3 == 0 || (x < 7 && y < 7 && (x == 0 || y == 0 || x == 6 || y == 6 || (x > 1 && x < 5 && y > 1 && y < 5))) {
				fmt.Fprintf(&c, "%d %d 1 1 re f\n", 2+x, int(hMM)-23+y)
			}
		}
	}
	fmt.Fprintf(&c, "BT /F1 3 Tf 2 2 Td (%s) Tj ET\n", text)
	content := c.String()

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>", wMM/25.4*72, hMM/25.4*72),
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
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
