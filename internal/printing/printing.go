// Package printing runs a print job end to end: printer status → media → PDF → raster → driver.
// It is shared by the CLI and the local server. Everything printer-specific lives in the
// driver (internal/driver/...), chosen through the device database (internal/devicedb).
package printing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	_ "github.com/francisrath/partdb-print-agent/internal/driver/brother" // registers the brother family
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/transport"
	_ "github.com/francisrath/partdb-print-agent/internal/transport/tcp" // registers kind tcp
	_ "github.com/francisrath/partdb-print-agent/internal/transport/usb" // registers kind usb
)

// Source returns the PDF to print once the media is known.
type Source func(m *driver.Media) ([]byte, error)

func PDF(pdf []byte) Source { return func(*driver.Media) ([]byte, error) { return pdf, nil } }

// TestCard is a test label sized for whatever media is loaded.
func TestCard(model, version string) Source {
	return func(m *driver.Media) ([]byte, error) {
		length := m.LengthMM
		if length == 0 {
			length = 40 // continuous: any length will do
		}
		lines := []string{"pdb-print test", model, m.ID, fmt.Sprintf("%d dots", m.PrintableWidthDots), version}
		if m.WidthDots > m.PrintableWidthDots { // tape: card on the print area, edges left clear
			return pdfgen.TestCardOn(m.WidthMM, float64(m.PrintableWidthDots)/float64(m.DPI)*25.4, length, lines), nil
		}
		return pdfgen.TestCard(m.WidthMM, length, lines), nil
	}
}

// Result is what a print did.
type Result = driver.Result

// Service renders with pdftoppm when set, else (or when it fails) with one PDFium renderer
// kept warm, and serialises jobs per printer.
type Service struct {
	Log      *slog.Logger
	Pdftoppm *render.Pdftoppm // nil = always PDFium
	// OpenTransport returns the connection to a printer; nil = by transport kind.
	// Tests put a fake printer here.
	OpenTransport func(p *config.Printer) driver.Transport
	// OnlineCheck replaces the reachability check behind Online (tests); nil = real check.
	OnlineCheck func(p *config.Printer) bool

	mu       sync.Mutex // guards r, printers and online
	online   map[string]onlineState
	probes   map[string]*sync.Mutex // one online check per printer at a time
	printing map[string]bool        // printers with a job running (their lock is also held by status checks and probes)

	beforePublish   func()                               // tests: between an online check returning and its result being stored
	probeTimeout    time.Duration                        // USB online check; 0 = 2 s
	openDevice      func(path string) (io.Closer, error) // USB online check; nil = open read/write
	waitingForCheck func()                               // tests: an online call is about to wait for a running check
	r               *render.Renderer
	printers        map[string]*sync.Mutex
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r != nil {
		_ = s.r.Close()
		s.r = nil
	}
}

func (s *Service) printerLock(id string) *sync.Mutex { return s.lockFor(&s.printers, id) }

func (s *Service) lockFor(locks *map[string]*sync.Mutex, id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *locks == nil {
		*locks = map[string]*sync.Mutex{}
	}
	if (*locks)[id] == nil {
		(*locks)[id] = &sync.Mutex{}
	}
	return (*locks)[id]
}

// render rasterises pdf for m. Media wider than its print area (tape) is rendered at full
// width and the edges trimmed; trimmed ink comes back as a warning.
func (s *Service) render(pdf []byte, p *config.Printer, m *driver.Media) ([]*image.Gray, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	width := max(m.WidthDots, m.PrintableWidthDots)
	opt := render.Options{
		WidthDots:     width,
		LengthDots:    m.PrintableLengthDots,
		MediaWidthMM:  m.WidthMM,
		MediaLengthMM: m.LengthMM,
		CheckSize:     !p.AllowScaling,
		Rotate:        p.Rotate,
		Threshold:     p.Threshold,
	}
	var pages []*image.Gray
	err := render.ErrPdftoppm
	if s.Pdftoppm != nil {
		if pages, err = s.Pdftoppm.Render(pdf, opt); errors.Is(err, render.ErrPdftoppm) {
			s.Log.Warn("pdftoppm failed, rendering with PDFium", "err", err)
		}
	}
	if errors.Is(err, render.ErrPdftoppm) {
		if s.r == nil {
			r, err := render.New()
			if err != nil {
				return nil, nil, err
			}
			s.r = r
		}
		pages, err = s.r.Render(pdf, opt)
	}
	if err != nil || width == m.PrintableWidthDots {
		return pages, nil, err
	}
	inked := false
	for i, pg := range pages {
		var lost bool
		pages[i], lost = trimEdges(pg, m.PrintableWidthDots)
		inked = inked || lost
	}
	if inked {
		edge := float64(width-m.PrintableWidthDots) / 2 / float64(m.DPI) * 25.4
		return pages, []string{fmt.Sprintf("label has ink on the tape's unprintable edges (%.1f mm); it was trimmed", edge)}, nil
	}
	return pages, nil, nil
}

// trimEdges keeps the centre printable columns of img; lost reports dark pixels cut off.
func trimEdges(img *image.Gray, printable int) (out *image.Gray, lost bool) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	off := (w - printable) / 2
	out = image.NewGray(image.Rect(0, 0, printable, h))
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w]
		copy(out.Pix[y*out.Stride:], row[off:off+printable])
		for x, v := range row {
			if v < 128 && (x < off || x >= off+printable) {
				lost = true
				break
			}
		}
	}
	return out, lost
}

// Render rasterises pdf for media without printing (previews).
func (s *Service) Render(pdf []byte, p *config.Printer, m *driver.Media) ([]*image.Gray, error) {
	pages, warnings, err := s.render(pdf, p, m)
	for _, w := range warnings {
		s.Log.Warn(w, "media", m.ID)
	}
	return pages, err
}

// LookupMedia finds a media id the printer's model can print; with no (known) model, any
// media in the device database.
func LookupMedia(p *config.Printer, id string) (driver.Media, bool) {
	db := devicedb.Default()
	if m, ok := db.Model(p.Model); ok {
		for _, md := range db.MediaFor(m) {
			if md.ID == id {
				return md, true
			}
		}
		return driver.Media{}, false
	}
	return db.Media(id)
}

// CheckDrivers reports printers whose model has no driver in this build (the config
// package can't see the driver registry).
func CheckDrivers(cfg *config.Config) error {
	for i := range cfg.Printers {
		p := &cfg.Printers[i]
		m, err := p.DeviceModel()
		if err != nil {
			return fmt.Errorf("printer %q: %w", p.ID, err)
		}
		if !slices.Contains(driver.Families(), m.Driver) {
			return fmt.Errorf("printer %q: no driver for %s in this build", p.ID, m.ID)
		}
	}
	return nil
}

func options(p *config.Printer) driver.Options {
	return driver.Options{Host: p.Transport.Host, SNMPCommunity: p.SNMPCommunity, NoSNMP: p.NoSNMP}
}

// lazyTransport opens the connection on first use, so a driver that gets its answer elsewhere
// (a QL over SNMP) never connects. Open errors come back from the Read/Write that triggered them.
type lazyTransport struct {
	ctx    context.Context
	t      driver.Transport
	opened bool
	err    error
}

func (l *lazyTransport) open() error {
	if !l.opened {
		l.opened, l.err = true, l.t.Open(l.ctx)
	}
	return l.err
}

func (l *lazyTransport) Open(context.Context) error { return l.open() }
func (l *lazyTransport) Kind() string               { return l.t.Kind() }

func (l *lazyTransport) Write(p []byte) (int, error) {
	if err := l.open(); err != nil {
		return 0, err
	}
	return l.t.Write(p)
}

func (l *lazyTransport) Read(p []byte) (int, error) {
	if err := l.open(); err != nil {
		return 0, err
	}
	return l.t.Read(p)
}

func (l *lazyTransport) Close() error {
	if l.opened && l.err == nil {
		return l.t.Close()
	}
	return nil
}

func (s *Service) connect(ctx context.Context, p *config.Printer) (*lazyTransport, error) {
	if s.OpenTransport != nil {
		return &lazyTransport{ctx: ctx, t: s.OpenTransport(p)}, nil
	}
	t, err := transport.New(p.Transport)
	if err != nil {
		return nil, err
	}
	return &lazyTransport{ctx: ctx, t: t}, nil
}

// printer resolves the printer's model and builds its driver.
func printer(p *config.Printer) (*devicedb.Model, driver.Driver, error) {
	m, err := p.DeviceModel()
	if err != nil {
		return nil, nil, err
	}
	d, err := driver.New(m, options(p))
	return m, d, err
}

// commsError maps errors that aren't the driver's own (e.g. failing to connect) to COMMS_ERROR.
func commsError(err error) error {
	var de *driver.Error
	if err == nil || errors.Is(err, driver.ErrNoReadback) || errors.As(err, &de) || errors.Is(err, context.Canceled) {
		return err
	}
	return &driver.Error{Code: driver.CommsError, Detail: err.Error()}
}

// Status queries the printer. Models that must not be asked while printing (PT) answer BUSY
// during a job without being asked. driver.ErrNoReadback if the printer doesn't answer.
func (s *Service) Status(ctx context.Context, p *config.Printer) (*driver.Status, error) {
	m, drv, err := printer(p)
	if err != nil {
		return nil, err
	}
	// a printing PT must not be asked; a USB device opens only once at a time (usblp)
	if m.QuirkString("status_while_printing") == "busy" || p.Transport.Kind == "usb" {
		lock := s.printerLock(p.ID)
		if !lock.TryLock() {
			return &driver.Status{Code: driver.Busy, Detail: "printing a job", Model: m.ID}, nil
		}
		defer lock.Unlock()
	}
	t, err := s.connect(ctx, p)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	st, err := drv.Status(ctx, t)
	return st, commsError(err)
}

// Print checks the printer, settles the media (config or what the printer reports), gets the
// PDF from source, renders it and hands it to the driver, which sends it and waits for the
// printer to confirm. With dryRun set, the byte stream goes to that file instead.
func (s *Service) Print(ctx context.Context, p *config.Printer, dryRun string, source Source) (*Result, error) {
	lock := s.printerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	s.setPrinting(p.ID, true)
	defer s.setPrinting(p.ID, false)

	model, drv, err := printer(p)
	if err != nil {
		return nil, err
	}
	media, ok := LookupMedia(p, p.Media)
	if p.Media != "" && !ok {
		return nil, fmt.Errorf("unknown media %q", p.Media)
	}
	var t *lazyTransport
	var st *driver.Status
	if dryRun == "" {
		if t, err = s.connect(ctx, p); err != nil {
			return nil, err
		}
		defer t.Close()
		st, err = drv.Status(ctx, t)
		switch {
		case errors.Is(err, driver.ErrNoReadback) && model.QuirkBool("needs_status"):
			return nil, &driver.Error{Code: driver.CommsError, Detail: "printer sent no status reply"}
		case errors.Is(err, driver.ErrNoReadback):
			s.Log.Warn("printer sent no status; printing blind", "printer", p.ID)
			st = nil
		case err != nil:
			return nil, commsError(err)
		}
		// connect now, before fetching and rendering, even if the status came over SNMP
		if err := t.open(); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		if st != nil {
			s.Log.Info("printer status", "model", st.Model, "code", st.Code, "media", MediaName(st.Media), "type", st.MediaType, "via", st.Via)
			if st.Code != driver.OK {
				return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
			}
			switch {
			case st.Media == nil:
			case st.Media.ID == "unknown":
				s.Log.Warn("printer reports media this agent doesn't know; using the configured media", "loaded", MediaName(st.Media))
			case p.Media != "" && st.Media.ID != p.Media:
				return nil, &driver.Error{Code: driver.MediaMismatch,
					Detail: fmt.Sprintf("config says %s, printer has %s loaded", p.Media, MediaName(st.Media))}
			case p.Media == "":
				media, ok = *st.Media, true
			}
		}
	}
	if !ok {
		return nil, errors.New("media unknown: set `media:` for this printer in the config")
	}

	pdf, err := source(&media)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	pages, warnings, err := s.render(pdf, p, &media)
	if errors.Is(err, render.ErrSize) {
		return nil, &driver.Error{Code: driver.MediaMismatch, Detail: err.Error() + " (set allow_scaling to print it scaled)"}
	}
	if err != nil {
		return nil, &driver.Error{Code: driver.RenderError, Detail: err.Error()}
	}
	s.Log.Info("rendered", "pages", len(pages), "media", media.ID, "took", time.Since(start).Round(time.Millisecond))
	for _, w := range warnings {
		s.Log.Warn(w, "printer", p.ID)
	}

	job := &driver.Job{Pages: pages, Media: &media, Cut: p.Cut, Status: st}
	if dryRun != "" {
		var b bytes.Buffer
		if err := drv.Encode(&b, job); err != nil {
			return nil, err
		}
		res := &Result{Pages: len(pages), Media: media.ID, Bytes: b.Len(), Warnings: warnings}
		return res, os.WriteFile(dryRun, b.Bytes(), 0o644)
	}
	res, err := drv.Print(ctx, t, job)
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		s.Log.Warn(w, "printer", p.ID)
	}
	res.Warnings = append(warnings, res.Warnings...)
	s.Log.Info(res.Message, "printer", p.ID, "pages", res.Pages, "bytes", res.Bytes, "took", time.Since(start).Round(time.Millisecond))
	return res, nil
}

func MediaName(m *driver.Media) string {
	if m == nil {
		return "none"
	}
	if m.ID == "unknown" {
		return fmt.Sprintf("unknown %s %gx%g mm", m.Kind, m.WidthMM, m.LengthMM)
	}
	return m.ID
}
