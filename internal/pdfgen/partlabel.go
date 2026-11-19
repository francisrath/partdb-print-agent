package pdfgen

import (
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/boombuler/barcode/code128"
	"github.com/boombuler/barcode/qr"
)

// PartLabel is a Part-DB-style part label of wMM×hMM: title and part number at the top, a QR code
// and a Code 128 drawn as vector rectangles (so a printer gets them through the raster path at its
// own resolution), and tag ("2/5") so a tester can say which label is which.
func PartLabel(wMM, hMM float64, title, partNo, qrText, barcode, tag string) []byte {
	const m = 2.5 // margin
	var c strings.Builder
	size := math.Max(1.6, math.Min(hMM/12, 6))
	y := hMM - m - size
	for _, s := range []string{title, partNo} {
		fs := math.Floor(math.Min(size, (wMM-2*m)/math.Max(textWidthEm(s), 0.001))*100) / 100
		fmt.Fprintf(&c, "BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n", fs, m, y, Text(s))
		y -= size * 1.25
	}
	barH := math.Max(4, math.Min(hMM*0.15, 14))
	// Code 128 across the bottom, 10 modules of quiet zone each side
	c.WriteString("% code128\n0 g\n")
	if bc, err := code128.Encode(barcode); err == nil {
		n := bc.Bounds().Dx()
		mod := (wMM - 2*m) / float64(n+20)
		drawRuns(&c, bc, m+10*mod, m, mod, barH, 1)
	}
	c.WriteString("% end\n")
	// QR code in the space between, 4 modules of quiet zone
	c.WriteString("% qr\n0 g\n")
	space := y + size*0.3 - (m + barH + 1)
	if code, err := qr.Encode(qrText, qr.M, qr.Auto); err == nil && space > 5 {
		n := code.Bounds().Dx()
		side := math.Min(space, (wMM-2*m)*0.6)
		mod := math.Floor(side/float64(n+8)*100) / 100
		top := m + barH + 1 + space - 4*mod
		drawRuns(&c, code, m+4*mod, top-float64(n)*mod, mod, mod, n)
		if tag != "" {
			x := m + float64(n+8)*mod + 2
			fmt.Fprintf(&c, "BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n", size*1.6, math.Min(x, wMM-m-size*4), top-size*1.6, Text(tag))
		}
	} else if tag != "" {
		fmt.Fprintf(&c, "BT /F1 %.2f Tf %.2f %.2f Td (%s) Tj ET\n", size, wMM-m-size*3, y, Text(tag))
	}
	c.WriteString("% end\n")
	return Page(wMM, hMM, c.String())
}

// drawRuns draws the dark modules of img as rectangles, one per horizontal run, with the image's
// top row at the top: x0, y0 is the bottom left corner, mod the module width, rowH a row's height.
func drawRuns(c *strings.Builder, img image.Image, x0, y0, mod, rowH float64, rows int) {
	b := img.Bounds()
	for y := 0; y < rows; y++ {
		yy := y0 + float64(rows-1-y)*rowH
		for x := 0; x < b.Dx(); {
			if !dark(img, b.Min.X+x, b.Min.Y+min(y, b.Dy()-1)) {
				x++
				continue
			}
			start := x
			for x < b.Dx() && dark(img, b.Min.X+x, b.Min.Y+min(y, b.Dy()-1)) {
				x++
			}
			fmt.Fprintf(c, "%.3f %.3f %.3f %.3f re f\n", x0+float64(start)*mod, yy, float64(x-start)*mod, rowH)
		}
	}
}

func dark(img image.Image, x, y int) bool {
	r, _, _, _ := img.At(x, y).RGBA()
	return r < 0x8000
}
