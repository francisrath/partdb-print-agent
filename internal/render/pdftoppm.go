package render

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ErrPdftoppm wraps failures of the poppler tools themselves (missing, crashed, odd output),
// after which the caller should fall back to PDFium. Size and limit errors are not wrapped.
var ErrPdftoppm = errors.New("pdftoppm")

// Pdftoppm renders with poppler's pdftoppm and pdfinfo, run as separate programs.
// Much faster than PDFium-in-wasm where wazero can only interpret (32-bit ARM).
// Poppler is GPL: it must only ever be executed like this, never linked or vendored.
type Pdftoppm struct {
	Bin, Info string        // paths of pdftoppm and pdfinfo
	Timeout   time.Duration // per program run; 0 = 2 minutes
	maxPixels int
}

// interpreted: wazero has no compiler here, so PDFium takes about a minute per label.
var interpreted = runtime.GOARCH == "arm" || runtime.GOARCH == "386"

// FindPdftoppm resolves setting: "" or "auto" uses pdftoppm from PATH where PDFium is
// interpreted (32-bit ARM and x86), "on" uses it from PATH everywhere, "off" never;
// anything else is the path of pdftoppm. nil, nil = use PDFium. pdfinfo is taken from
// pdftoppm's directory, else from PATH.
func FindPdftoppm(setting string) (*Pdftoppm, error) {
	return findPdftoppm(setting, interpreted)
}

func findPdftoppm(setting string, interpreted bool) (*Pdftoppm, error) {
	bin := setting
	switch setting {
	case "off":
		return nil, nil
	case "", "auto", "on":
		if setting != "on" && !interpreted {
			return nil, nil
		}
		p, err := exec.LookPath("pdftoppm")
		if err != nil {
			return nil, nil
		}
		bin = p
	}
	bin, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("pdftoppm: %w", err)
	}
	name := "pdfinfo"
	if ext := filepath.Ext(bin); strings.EqualFold(ext, ".exe") {
		name += ext
	}
	info, err := exec.LookPath(filepath.Join(filepath.Dir(bin), name))
	if err != nil {
		if info, err = exec.LookPath("pdfinfo"); err != nil {
			return nil, fmt.Errorf("pdftoppm found at %s but no pdfinfo: %w", bin, err)
		}
	}
	return &Pdftoppm{Bin: bin, Info: info}, nil
}

// Render has the same contract as Renderer.Render.
func (p *Pdftoppm) Render(pdf []byte, o Options) ([]*image.Gray, error) {
	if err := check(pdf, &o); err != nil {
		return nil, err
	}
	sizes, err := p.pageSizes(pdf)
	if err != nil {
		return nil, err
	}
	if len(sizes) > MaxPages {
		return nil, fmt.Errorf("PDF has %d pages, max %d", len(sizes), MaxPages)
	}
	type page struct {
		rot, w, h int
		turned    bool
	}
	pages := make([]page, len(sizes))
	maxPixels := p.maxPixels
	if maxPixels == 0 {
		maxPixels = MaxPixels
	}
	budget := maxPixels
	for i, s := range sizes {
		rot, w, h, err := layout(s.w, s.h, o, &budget, maxPixels)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", i+1, err)
		}
		pages[i] = page{int(rot), w, h, s.turned}
	}
	out := make([]*image.Gray, 0, len(pages))
	// One pdftoppm run per stretch of pages that share a layout (usually the whole PDF).
	for first := 0; first < len(pages); {
		last := first
		for last+1 < len(pages) && pages[last+1] == pages[first] {
			last++
		}
		pg := pages[first]
		// pdftoppm renders the page upright; rotate afterwards (lossless for quarter turns).
		sx, sy := pg.w, pg.h
		if pg.rot%2 == 1 {
			sx, sy = sy, sx
		}
		// -scale-to-x/y apply before the page's own /Rotate (-scale-dimension-before-rotation
		// would flip that, but older poppler lacks it).
		ax, ay := sx, sy
		if pg.turned {
			ax, ay = ay, ax
		}
		n := last - first + 1
		b, err := p.run(pdf, n*(sx*sy+64), p.Bin, "-cropbox", "-gray", "-aa", "yes", "-aaVector", "no",
			"-f", strconv.Itoa(first+1), "-l", strconv.Itoa(last+1),
			"-scale-to-x", strconv.Itoa(ax), "-scale-to-y", strconv.Itoa(ay), "-")
		if err != nil {
			return nil, err
		}
		imgs, err := parsePGMs(b, n, sx, sy)
		if err != nil {
			return nil, fmt.Errorf("%w: pages %d-%d: %v", ErrPdftoppm, first+1, last+1, err)
		}
		for _, img := range imgs {
			for i, v := range img.Pix {
				if v < o.Threshold {
					img.Pix[i] = 0
				} else {
					img.Pix[i] = 255
				}
			}
			out = append(out, rotateCW(img, pg.rot))
		}
		first = last + 1
	}
	return out, nil
}

// run executes a poppler tool with pdf on stdin (no shell, no temp file) and returns its
// stdout, failing once that exceeds maxOut bytes.
func (p *Pdftoppm) run(pdf []byte, maxOut int, bin string, args ...string) ([]byte, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(pdf)
	out := &capped{max: maxOut}
	stderr := &tail{n: 300} // a broken PDF can make poppler print a warning per operator
	cmd.Stdout, cmd.Stderr = out, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s: %v %s", ErrPdftoppm, filepath.Base(bin), err, strings.TrimSpace(string(stderr.b)))
	}
	return out.Bytes(), nil
}

var errTooMuchOutput = errors.New("more output than expected")

// capped collects up to max bytes and then fails the write (and so the program).
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(b []byte) (int, error) {
	if c.Len()+len(b) > c.max {
		return 0, errTooMuchOutput
	}
	return c.Buffer.Write(b)
}

// tail keeps only the last n bytes written to it.
type tail struct {
	n int
	b []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.b = append(t.b, b...)
	if len(t.b) > t.n {
		t.b = append(t.b[:0], t.b[len(t.b)-t.n:]...)
	}
	return len(b), nil
}

var (
	reInfoPages = regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`)
	reInfoSize  = regexp.MustCompile(`(?m)^Page\s+(\d+) size:\s+([\d.]+) x ([\d.]+) pts`)
	reInfoRot   = regexp.MustCompile(`(?m)^Page\s+(\d+) rot:\s+(\d+)`)
)

// pageSize is a page's displayed size in points (crop box, /Rotate applied), the same
// numbers PDFium's FPDF_GetPageSizeByIndex gives; turned = /Rotate is 90 or 270.
type pageSize struct {
	w, h   float64
	turned bool
}

func (p *Pdftoppm) pageSizes(pdf []byte) ([]pageSize, error) {
	b, err := p.run(pdf, 1<<20, p.Info, "-f", "1", "-l", strconv.Itoa(MaxPages+1), "-")
	if err != nil {
		return nil, err
	}
	sizes, err := parseInfo(string(b))
	if err != nil {
		return nil, fmt.Errorf("%w: pdfinfo: %v", ErrPdftoppm, err)
	}
	return sizes, nil
}

func parseInfo(s string) ([]pageSize, error) {
	m := reInfoPages.FindStringSubmatch(s)
	if m == nil {
		return nil, errors.New("no page count")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return nil, fmt.Errorf("bad page count %q", m[1])
	}
	if n > MaxPages {
		n = MaxPages + 1 // enough for the caller's limit check; pdfinfo was asked for no more
	}
	sizes := make([]pageSize, n)
	seen := 0
	for _, m := range reInfoSize.FindAllStringSubmatch(s, -1) {
		i, _ := strconv.Atoi(m[1])
		w, err1 := strconv.ParseFloat(m[2], 64)
		h, err2 := strconv.ParseFloat(m[3], 64)
		if i < 1 || i > n || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			return nil, fmt.Errorf("bad size line %q", m[0])
		}
		sizes[i-1] = pageSize{w: w, h: h}
		seen++
	}
	if seen != n {
		return nil, fmt.Errorf("%d page sizes for %d pages", seen, n)
	}
	for _, m := range reInfoRot.FindAllStringSubmatch(s, -1) {
		i, _ := strconv.Atoi(m[1])
		r, _ := strconv.Atoi(m[2])
		if i >= 1 && i <= n && (r/90)%2 == 1 {
			sizes[i-1] = pageSize{w: sizes[i-1].h, h: sizes[i-1].w, turned: true}
		}
	}
	return sizes, nil
}

// parsePGMs reads n concatenated binary PGMs ("P5"), each exactly w×h with maxval 255.
func parsePGMs(b []byte, n, w, h int) ([]*image.Gray, error) {
	r := bufio.NewReader(bytes.NewReader(b))
	out := make([]*image.Gray, 0, n)
	for i := 0; i < n; i++ {
		var magic string
		var pw, ph, maxval int
		// Fscan skips whitespace; exactly one whitespace byte separates maxval from the pixels.
		if _, err := fmt.Fscan(r, &magic, &pw, &ph, &maxval); err != nil {
			return nil, fmt.Errorf("image %d header: %v", i+1, err)
		}
		if magic != "P5" || maxval != 255 {
			return nil, fmt.Errorf("image %d: not an 8-bit PGM (%s, maxval %d)", i+1, magic, maxval)
		}
		if pw != w || ph != h {
			return nil, fmt.Errorf("image %d is %dx%d, want %dx%d", i+1, pw, ph, w, h)
		}
		if _, err := r.ReadByte(); err != nil {
			return nil, err
		}
		img := image.NewGray(image.Rect(0, 0, w, h))
		if _, err := io.ReadFull(r, img.Pix); err != nil {
			return nil, fmt.Errorf("image %d: %v", i+1, err)
		}
		out = append(out, img)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return nil, errors.New("trailing data after the last image")
	}
	return out, nil
}

// rotateCW turns img clockwise by q quarter turns.
func rotateCW(img *image.Gray, q int) *image.Gray {
	q &= 3
	if q == 0 {
		return img
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	dw, dh := h, w
	if q == 2 {
		dw, dh = w, h
	}
	out := image.NewGray(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch q {
			case 1:
				dx, dy = h-1-y, x
			case 2:
				dx, dy = w-1-x, h-1-y
			case 3:
				dx, dy = y, w-1-x
			}
			out.Pix[dy*dw+dx] = img.Pix[y*w+x]
		}
	}
	return out
}
