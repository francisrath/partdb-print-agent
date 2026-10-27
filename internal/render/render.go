// Package render rasterises PDF pages to 1-bit images at exact printhead dots
// using PDFium compiled to WebAssembly (no cgo, sandboxed, no filesystem access).
package render

import (
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

const (
	MaxPDFBytes = 10 << 20
	MaxPages    = 500
	// MaxPixels bounds the rasters one Render call may hold (1 byte per dot): 256 MiB,
	// i.e. about 31 m of 62 mm tape. Checked before each page's bitmap is allocated.
	MaxPixels = 256 << 20
	mmPerPt   = 25.4 / 72
	// A page side within this of the media width is treated as the cross-feed side.
	widthToleranceMM = 0.6
)

// ErrSize means the page doesn't fit the media (see Options.CheckSize).
var ErrSize = errors.New("label size doesn't match the media")

type Options struct {
	WidthDots    int     // cross-feed (printhead) dots
	LengthDots   int     // feed dots; 0 = keep the page's aspect ratio (continuous media)
	MediaWidthMM float64 // used by Rotate "auto" and CheckSize
	// CheckSize refuses pages that don't fit the media: the cross-feed side must be within
	// ±0.6 mm of MediaWidthMM and, for die-cut (MediaLengthMM > 0), the length within ±1 mm.
	CheckSize     bool
	MediaLengthMM float64
	Rotate        string // "" or "auto", "0", "90", "180", "270" (clockwise)
	Threshold     uint8  // luminance below this is black; 0 = 128
}

type Renderer struct {
	pool      pdfium.Pool
	instance  pdfium.Pdfium
	maxPixels int // MaxPixels; lowered in tests
}

// ReadFile reads a PDF from disk without ever holding more than MaxPDFBytes+1 bytes,
// so an oversized file is refused before it is read whole.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxPDFBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPDFBytes {
		return nil, fmt.Errorf("%s: PDF larger than %d bytes", path, MaxPDFBytes)
	}
	return b, nil
}

// New compiles the PDFium wasm module; this takes a moment, so reuse the Renderer.
func New() (*Renderer, error) {
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle: 1, MaxIdle: 1, MaxTotal: 1,
		FSConfig: wazero.NewFSConfig(), // nothing mounted: PDFs only ever arrive as bytes
	})
	if err != nil {
		return nil, fmt.Errorf("init pdfium: %w", err)
	}
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("pdfium instance: %w", err)
	}
	return &Renderer{pool: pool, instance: inst, maxPixels: MaxPixels}, nil
}

func (r *Renderer) Close() error {
	_ = r.instance.Close()
	return r.pool.Close()
}

// Render returns one black/white image per page; pixel values are 0 (black) or 255.
// Rows run along the feed direction; columns are printhead dots.
// A Renderer is not safe for concurrent use.
func (r *Renderer) Render(pdf []byte, o Options) ([]*image.Gray, error) {
	if len(pdf) > MaxPDFBytes {
		return nil, fmt.Errorf("PDF too large (%d bytes, max %d)", len(pdf), MaxPDFBytes)
	}
	if o.WidthDots <= 0 {
		return nil, errors.New("render: WidthDots must be set")
	}
	if o.Threshold == 0 {
		o.Threshold = 128
	}
	p := r.instance
	doc, err := p.OpenDocument(&requests.OpenDocument{File: &pdf})
	if err != nil {
		return nil, fmt.Errorf("open PDF: %w", err)
	}
	defer p.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})

	cnt, err := p.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return nil, err
	}
	if cnt.PageCount > MaxPages {
		return nil, fmt.Errorf("PDF has %d pages, max %d", cnt.PageCount, MaxPages)
	}
	var out []*image.Gray
	budget := r.maxPixels
	for i := 0; i < cnt.PageCount; i++ {
		img, err := r.renderPage(doc.Document, i, o, &budget)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", i+1, err)
		}
		out = append(out, img)
	}
	return out, nil
}

func (r *Renderer) renderPage(doc references.FPDF_DOCUMENT, index int, o Options, budget *int) (*image.Gray, error) {
	p := r.instance
	size, err := p.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc, Index: index})
	if err != nil {
		return nil, err
	}
	wMM, hMM := size.Width*mmPerPt, size.Height*mmPerPt
	rot, err := rotation(o.Rotate, wMM, hMM, o.MediaWidthMM)
	if err != nil {
		return nil, err
	}
	crossMM, feedMM := wMM, hMM
	if rot == enums.FPDF_PAGE_ROTATION_90_CW || rot == enums.FPDF_PAGE_ROTATION_270_CW {
		crossMM, feedMM = hMM, wMM
	}
	if o.CheckSize && (math.Abs(crossMM-o.MediaWidthMM) > widthToleranceMM ||
		(o.MediaLengthMM > 0 && math.Abs(feedMM-o.MediaLengthMM) > 1)) {
		media := fmt.Sprintf("%g mm", o.MediaWidthMM)
		if o.MediaLengthMM > 0 {
			media = fmt.Sprintf("%g×%g mm", o.MediaWidthMM, o.MediaLengthMM)
		}
		return nil, fmt.Errorf("%w: label is %.1f×%.1f mm, loaded media is %s", ErrSize, wMM, hMM, media)
	}
	// Render straight at the target dot size: resampling afterwards would smear QR modules.
	w, h := o.WidthDots, o.LengthDots
	if h == 0 {
		h = int(math.Round(feedMM * float64(w) / crossMM))
	}
	if h <= 0 || h > 100000 {
		return nil, fmt.Errorf("bad raster length %d", h)
	}
	if *budget -= w * h; *budget < 0 {
		return nil, fmt.Errorf("PDF too large to rasterise (over %d megapixels in total)", r.maxPixels>>20)
	}

	page, err := p.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc, Index: index})
	if err != nil {
		return nil, err
	}
	defer p.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: page.Page})

	bm, err := p.FPDFBitmap_Create(&requests.FPDFBitmap_Create{Width: w, Height: h, Alpha: 0})
	if err != nil {
		return nil, err
	}
	defer p.FPDFBitmap_Destroy(&requests.FPDFBitmap_Destroy{Bitmap: bm.Bitmap})

	if _, err := p.FPDFBitmap_FillRect(&requests.FPDFBitmap_FillRect{Bitmap: bm.Bitmap, Width: w, Height: h, Color: 0xFFFFFFFF}); err != nil {
		return nil, err
	}
	flags := enums.FPDF_RENDER_FLAG_ANNOT | enums.FPDF_RENDER_FLAG_PRINTING |
		enums.FPDF_RENDER_FLAG_RENDER_NO_SMOOTHTEXT | enums.FPDF_RENDER_FLAG_RENDER_NO_SMOOTHIMAGE |
		enums.FPDF_RENDER_FLAG_RENDER_NO_SMOOTHPATH
	if _, err := p.FPDF_RenderPageBitmap(&requests.FPDF_RenderPageBitmap{
		Bitmap: bm.Bitmap, Page: requests.Page{ByReference: &page.Page},
		SizeX: w, SizeY: h, Rotate: rot, Flags: flags,
	}); err != nil {
		return nil, err
	}
	stride, err := p.FPDFBitmap_GetStride(&requests.FPDFBitmap_GetStride{Bitmap: bm.Bitmap})
	if err != nil {
		return nil, err
	}
	buf, err := p.FPDFBitmap_GetBuffer(&requests.FPDFBitmap_GetBuffer{Bitmap: bm.Bitmap})
	if err != nil {
		return nil, err
	}
	return threshold(buf.Buffer, stride.Stride, w, h, o.Threshold), nil
}

// threshold converts BGRx pixels to pure black/white. No dithering: barcodes stay hard-edged.
func threshold(bgrx []byte, stride, w, h int, t uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := bgrx[y*stride:]
		for x := 0; x < w; x++ {
			b, g, r := uint32(row[4*x]), uint32(row[4*x+1]), uint32(row[4*x+2])
			lum := (299*r + 587*g + 114*b) / 1000
			if lum >= uint32(t) {
				img.Pix[y*img.Stride+x] = 255
			}
		}
	}
	return img
}

// rotation decides the clockwise rotation for a page of wMM×hMM.
// Auto: the side matching the media width goes across the head; failing that,
// the long side runs along the tape.
func rotation(rot string, wMM, hMM, mediaMM float64) (enums.FPDF_PAGE_ROTATION, error) {
	switch rot {
	case "0":
		return enums.FPDF_PAGE_ROTATION_NONE, nil
	case "90":
		return enums.FPDF_PAGE_ROTATION_90_CW, nil
	case "180":
		return enums.FPDF_PAGE_ROTATION_180_CW, nil
	case "270":
		return enums.FPDF_PAGE_ROTATION_270_CW, nil
	case "", "auto":
	default:
		return 0, fmt.Errorf("bad rotate %q", rot)
	}
	near := func(a float64) bool { return mediaMM > 0 && math.Abs(a-mediaMM) <= widthToleranceMM }
	turn := wMM > hMM
	if near(wMM) != near(hMM) {
		turn = near(hMM)
	}
	if turn {
		// 90° counter-clockwise, the same direction as the proven qlprint/brother_ql path.
		return enums.FPDF_PAGE_ROTATION_270_CW, nil
	}
	return enums.FPDF_PAGE_ROTATION_NONE, nil
}
