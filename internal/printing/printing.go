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
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
	_ "github.com/francisrath/pdb-print/internal/driver/brother" // registers the brother family
	_ "github.com/francisrath/pdb-print/internal/driver/cat"     // registers the cat family (BLE cat printers)
	_ "github.com/francisrath/pdb-print/internal/driver/dymo"    // registers the dymo family (LabelWriter)
	_ "github.com/francisrath/pdb-print/internal/driver/epl"     // registers the epl family (Zebra EPL2)
	_ "github.com/francisrath/pdb-print/internal/driver/escpos"  // registers the escpos family (SPP receipt printers)
	_ "github.com/francisrath/pdb-print/internal/driver/niimbot" // registers the niimbot family (BLE label makers)
	_ "github.com/francisrath/pdb-print/internal/driver/phomemo" // registers the phomemo family (BLE label printers)
	_ "github.com/francisrath/pdb-print/internal/driver/tspl"    // registers the tspl family (TSC and clones)
	_ "github.com/francisrath/pdb-print/internal/driver/zpl"     // registers the zpl family (Zebra)
	"github.com/francisrath/pdb-print/internal/partdb"
	"github.com/francisrath/pdb-print/internal/pdfgen"
	"github.com/francisrath/pdb-print/internal/render"
	"github.com/francisrath/pdb-print/internal/transport"
	_ "github.com/francisrath/pdb-print/internal/transport/ble"    // registers kind ble (in-process on Linux/Windows, via pdb-print-ble on macOS)
	_ "github.com/francisrath/pdb-print/internal/transport/bt"     // registers kind bt (Linux/Windows; macOS: transport/ble)
	_ "github.com/francisrath/pdb-print/internal/transport/serial" // registers kind serial (Linux, macOS, Windows)
	_ "github.com/francisrath/pdb-print/internal/transport/tcp"    // registers kind tcp
	_ "github.com/francisrath/pdb-print/internal/transport/usb"    // registers kind usb
)

// Source returns the PDF to print once the media is known.
type Source func(m *devicedb.Media) ([]byte, error)

func PDF(pdf []byte) Source { return func(*devicedb.Media) ([]byte, error) { return pdf, nil } }

// TestCard is a test label sized for whatever media is loaded.
func TestCard(model, version string) Source { return TestCardTagged(model, version, "") }

// TestCardTagged is TestCard with a tag ("1/5") printed large, for numbered test runs.
func TestCardTagged(model, version, tag string) Source {
	return func(m *devicedb.Media) ([]byte, error) {
		length := m.LengthMM
		if length == 0 {
			length = 40 // continuous: any length will do
		}
		lines := []string{"pdb-print test", model, m.ID, fmt.Sprintf("%d dots", m.PrintableWidthDots), version}
		if m.WidthDots > m.PrintableWidthDots { // tape: card on the print area, edges left clear
			return pdfgen.TestCardOn(m.WidthMM, float64(m.PrintableWidthDots)/float64(m.DPI)*25.4, length, lines), nil
		}
		return pdfgen.TestCardTagged(m.WidthMM, length, lines, tag), nil
	}
}

// PartLabel is a generated part label (title, part number, QR code, Code 128) at the media's size.
func PartLabel(tag string) Source {
	return func(m *devicedb.Media) ([]byte, error) {
		length := m.LengthMM
		if length == 0 {
			length = max(m.WidthMM*0.6, 20)
		}
		return pdfgen.PartLabel(m.WidthMM, length, "AMS1117-3.3 LDO regulator", "P-000032",
			"https://partdb.example.org/en/part/32", "P-000032", tag), nil
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

	mu          sync.Mutex // guards r, printers, online, unconfirmed, seen, media and profileSizes
	online      map[string]onlineState
	probes      map[string]*sync.Mutex // one online check per printer at a time
	printing    map[string]bool        // printers with a job running (their lock is also held by status checks and probes)
	unconfirmed map[string]bool        // printers whose last job was sent but not confirmed (see driver.Job.PrevUnconfirmed)
	seen        map[string]time.Time   // BLE printers (by onlineKey): when a status, job or scan last found them working
	media       map[string]knownMedia  // last roll each printer reported
	// label profile sizes measured by ProfilesFitting, by Part-DB URL and profile id
	profileSizes map[string]profileSize

	beforePublish   func()                                                       // tests: between an online check returning and its result being stored
	probeTimeout    time.Duration                                                // USB online check; 0 = 2 s
	openDevice      func(path string) (io.Closer, error)                         // USB online check; nil = open read/write
	waitingForCheck func()                                                       // tests: an online call is about to wait for a running check
	newDriver       func(*devicedb.Model, driver.Options) (driver.Driver, error) // tests: a fake driver; nil = driver.New
	r               *render.Renderer
	printers        map[string]*sync.Mutex
	gate            sync.RWMutex // Print holds it for reading; Quiesce for writing
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
func (s *Service) render(pdf []byte, p *config.Printer, m *devicedb.Media) ([]*image.Gray, []string, error) {
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
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if width != m.PrintableWidthDots {
		inked := false
		for i, pg := range pages {
			var lost bool
			pages[i], lost = trimEdges(pg, m.PrintableWidthDots)
			inked = inked || lost
		}
		if inked {
			edge := float64(width-m.PrintableWidthDots) / 2 / float64(m.DPI) * 25.4
			warnings = []string{fmt.Sprintf("label has ink on the tape's unprintable edges (%.1f mm); it was trimmed", edge)}
		}
	}
	for _, pg := range pages {
		flip(pg, p.Mirror != p.UpsideDown, p.UpsideDown)
	}
	return pages, warnings, nil
}

// flip mirrors img in place left-right (h) and/or top-bottom (v); both = turned 180°.
func flip(img *image.Gray, h, v bool) {
	w, ht := img.Bounds().Dx(), img.Bounds().Dy()
	if h {
		for y := range ht {
			slices.Reverse(img.Pix[y*img.Stride : y*img.Stride+w])
		}
	}
	if v {
		for y := range ht / 2 {
			a, b := img.Pix[y*img.Stride:y*img.Stride+w], img.Pix[(ht-1-y)*img.Stride:(ht-1-y)*img.Stride+w]
			for x := range a {
				a[x], b[x] = b[x], a[x]
			}
		}
	}
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
func (s *Service) Render(pdf []byte, p *config.Printer, m *devicedb.Media) ([]*image.Gray, error) {
	pages, warnings, err := s.render(pdf, p, m)
	for _, w := range warnings {
		s.Log.Warn(w, "media", m.ID)
	}
	return pages, err
}

// LookupMedia finds a media id the printer's model can print; with no (known) model, any
// media in the device database.
func LookupMedia(p *config.Printer, id string) (devicedb.Media, bool) {
	db := devicedb.Default()
	if m, ok := db.Model(p.Model); ok {
		for _, md := range db.MediaFor(m) {
			if md.ID == id {
				return md, true
			}
		}
		return devicedb.Media{}, false
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
	return driver.Options{Host: p.Transport.Host, SNMPCommunity: p.SNMPCommunity, NoSNMP: p.NoSNMP, Darkness: p.Darkness, BlackMarkMM: p.BlackMarkMM,
		OffsetXDots: p.Adjust.XDots, OffsetYDots: p.Adjust.YDots, PrintWidthDots: p.Adjust.PrintWidthDots}
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

// Unwrap returns the underlying transport, for driver-specific code.
func (l *lazyTransport) Unwrap() driver.Transport { return l.t }

// SetReadTimeout passes a driver's read timeout on when the transport takes one (driver.ReadTimeoutSetter).
func (l *lazyTransport) SetReadTimeout(d time.Duration) {
	if s, ok := l.t.(driver.ReadTimeoutSetter); ok {
		s.SetReadTimeout(d)
	}
}

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

// Close ends the connection; the next Open, Write or Read connects again (the zpl driver uses a
// connection per query).
func (l *lazyTransport) Close() error {
	defer func() { l.opened, l.err = false, nil }()
	if l.opened && l.err == nil {
		return l.t.Close()
	}
	return nil
}

func (s *Service) connect(ctx context.Context, p *config.Printer, m *devicedb.Model) (*lazyTransport, error) {
	if s.OpenTransport != nil {
		return &lazyTransport{ctx: ctx, t: s.OpenTransport(p)}, nil
	}
	t, err := transport.New(p.Transport, m)
	if err != nil {
		return nil, err
	}
	return &lazyTransport{ctx: ctx, t: t}, nil
}

// printer resolves the printer's model and builds its driver.
func (s *Service) printer(p *config.Printer) (*devicedb.Model, driver.Driver, error) {
	m, err := p.DeviceModel()
	if err != nil {
		return nil, nil, err
	}
	newDriver := driver.New
	if s.newDriver != nil {
		newDriver = s.newDriver
	}
	d, err := newDriver(m, options(p))
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

// Status queries the printer. Models that must not be asked while printing (PT) and printers on a
// single connection (USB, Bluetooth) answer BUSY during a job without being asked. driver.ErrNoReadback
// if the printer doesn't answer.
func (s *Service) Status(ctx context.Context, p *config.Printer) (st *driver.Status, err error) {
	m, drv, err := s.printer(p)
	if err != nil {
		return nil, err
	}
	// a printing PT must not be asked; a USB device opens only once at a time (usblp), a serial port too
	// (opened exclusively); a Bluetooth printer takes one connection, so a second one would drop the job's link
	if m.QuirkString("status_while_printing") == "busy" || p.Transport.Kind == "usb" || p.Transport.Kind == "serial" || config.Bluetooth(p.Transport.Kind) {
		lock := s.printerLock(p.ID)
		if !lock.TryLock() {
			return &driver.Status{Code: driver.Busy, Detail: "printing a job", Model: m.ID}, nil
		}
		defer lock.Unlock()
	}
	t, err := s.connect(ctx, p, m)
	if err != nil {
		return nil, err
	}
	defer t.Close()
	st, err = drv.Status(ctx, t)
	s.noteBluetooth(p, err)
	switch {
	case err == nil:
		s.observe(p, m, st)
	case !errors.Is(err, driver.ErrNoReadback):
		s.forgetMedia(p.ID)
	}
	return st, commsError(err)
}

// observe records the roll a status read reports: an allowed, known roll is remembered;
// any other report replaces what was remembered (the roll changed or the printer is unhappy).
func (s *Service) observe(p *config.Printer, model *devicedb.Model, st *driver.Status) {
	switch {
	case st == nil || st.Media == nil && st.Code == driver.OK:
		// says nothing about the roll: keep what we know
	case st.Code == driver.OK && st.Media.ID != "unknown" && slices.Contains(p.AllowedMedia(model), st.Media.ID):
		s.rememberMedia(p.ID, *st.Media)
	default:
		s.forgetMedia(p.ID)
	}
}

// Quiesce waits for running jobs and holds back new ones until release is called (a self-update swaps
// the program files meanwhile). ctx bounds the wait; on error nothing stays held.
func (s *Service) Quiesce(ctx context.Context) (release func(), err error) {
	got := make(chan struct{})
	go func() { s.gate.Lock(); close(got) }()
	select {
	case <-got:
		return s.gate.Unlock, nil
	case <-ctx.Done():
		go func() { <-got; s.gate.Unlock() }()
		return nil, ctx.Err()
	}
}

// Print checks the printer, settles the media (what the printer reports, else the config's), gets the
// PDF from source, renders it and hands it to the driver, which sends it and waits for the
// printer to confirm. With dryRun set, the byte stream goes to that file instead.
func (s *Service) Print(ctx context.Context, p *config.Printer, dryRun string, source Source) (_ *Result, err error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	lock := s.printerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	s.setPrinting(p.ID, true)
	defer s.setPrinting(p.ID, false)

	model, drv, err := s.printer(p)
	if err != nil {
		return nil, err
	}
	var t *lazyTransport
	var st *driver.Status
	if dryRun == "" {
		// noteBluetooth only hears how talking to the printer went: a bad document says nothing about its reach
		if t, err = s.connect(ctx, p, model); err != nil {
			s.noteBluetooth(p, err)
			return nil, err
		}
		defer t.Close()
		st, err = drv.Status(ctx, t)
		s.noteBluetooth(p, err)
		switch {
		case errors.Is(err, driver.ErrNoReadback) && model.QuirkBool("needs_status"):
			detail := "printer sent no status reply"
			if err != driver.ErrNoReadback { // the driver says more (Zebra: what silence usually means)
				detail = err.Error()
			}
			s.forgetMedia(p.ID)
			return nil, &driver.Error{Code: driver.CommsError, Detail: detail}
		case errors.Is(err, driver.ErrNoReadback):
			s.Log.Warn("printer sent no status; printing blind", "printer", p.ID)
			st = nil
		case err != nil:
			s.forgetMedia(p.ID)
			return nil, commsError(err)
		}
		// connect now, before fetching and rendering, even if the status came over SNMP
		if err := t.open(); err != nil {
			s.forgetMedia(p.ID)
			err = &driver.Error{Code: driver.CommsError, Detail: err.Error()}
			s.noteBluetooth(p, err)
			return nil, err
		}
		s.observe(p, model, st)
		if st != nil {
			s.Log.Info("printer status", "model", st.Model, "code", st.Code, "media", MediaName(st.Media), "type", st.MediaType, "via", st.Via)
			if st.Code != driver.OK {
				return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
			}
		}
	}
	media, from, err := s.pickMedia(p, model, st)
	if err != nil {
		return nil, err
	}
	s.Log.Debug("media", "printer", p.ID, "media", media.ID, "from", from)

	pdf, err := source(&media)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	pages, warnings, err := s.render(pdf, p, &media)
	var se *render.SizeError
	if errors.As(err, &se) {
		detail := devicedb.Mismatch(se.WMM, se.HMM, &media, devicedb.Default().MediaFor(model))
		if p.Rotate != "" && p.Rotate != "auto" {
			detail = err.Error() // a fixed rotation decides which side goes across: say what it checked
		}
		return nil, &driver.Error{Code: driver.MediaMismatch, Detail: detail + " (set allow_scaling to print it scaled)"}
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
		res := &Result{Pages: len(pages), Media: media.ID, MediaFrom: from, Bytes: b.Len(), Warnings: warnings}
		return res, os.WriteFile(dryRun, b.Bytes(), 0o644)
	}
	s.mu.Lock()
	job.PrevUnconfirmed = s.unconfirmed[p.ID]
	s.mu.Unlock()
	res, err := drv.Print(ctx, t, job)
	if err == nil {
		// closing can fail when the last data never left (USB: usblp still holds it)
		if cerr := t.Close(); cerr != nil {
			err = &driver.Error{Code: driver.CommsError, Detail: cerr.Error()}
		}
	}
	s.noteBluetooth(p, err)
	s.mu.Lock()
	if s.unconfirmed == nil {
		s.unconfirmed = map[string]bool{}
	}
	s.unconfirmed[p.ID] = err != nil || res == nil || !res.Confirmed // a failed job may still be printing
	s.mu.Unlock()
	if err != nil {
		s.forgetMedia(p.ID) // the roll may be out or swapped: ask again next time
		return nil, err
	}
	for _, w := range res.Warnings {
		s.Log.Warn(w, "printer", p.ID)
	}
	res.Warnings, res.MediaFrom = append(warnings, res.Warnings...), from
	s.Log.Info(res.Message, "printer", p.ID, "pages", res.Pages, "bytes", res.Bytes, "took", time.Since(start).Round(time.Millisecond))
	return res, nil
}

// Where a job's media came from (see pickMedia).
const (
	FromPrinter  = "detected"
	FromCache    = "detected earlier"
	FromFallback = "config fallback"
)

// mediaTTL is how long a detected roll stands in for a status read that says nothing about the
// media (SNMP down, a printer that printed blind).
const mediaTTL = 30 * time.Second

type knownMedia struct {
	m  devicedb.Media
	at time.Time
}

func (s *Service) rememberMedia(id string, m devicedb.Media) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.media == nil {
		s.media = map[string]knownMedia{}
	}
	s.media[id] = knownMedia{m, time.Now()}
}

func (s *Service) forgetMedia(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.media, id)
}

// LastMedia is the roll the printer reported last, however long ago (for display).
func (s *Service) LastMedia(id string) (devicedb.Media, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.media[id]
	return k.m, k.at, ok
}

// pickMedia settles the media for a job: the roll the printer reports, else the one it reported
// in the last 30 s, else the config's media as a fallback.
func (s *Service) pickMedia(p *config.Printer, model *devicedb.Model, st *driver.Status) (devicedb.Media, string, error) {
	fallback, hasFallback := devicedb.Media{}, false
	if id := p.FallbackMedia(); id != "" {
		if fallback, hasFallback = LookupMedia(p, id); !hasFallback {
			return fallback, "", fmt.Errorf("unknown media %q", id)
		}
	}
	if st != nil && st.Media != nil {
		loaded := st.Media
		switch {
		case loaded.ID == "unknown" && loaded.Kind == devicedb.DieCut && loaded.PrintableLengthDots > 0 && hasFallback:
			// a printer that measures its labels but can't name them (Zebra): when it measured up to a
			// millimetre less than configured, print at that length so no row lands on the gap; the
			// measurement includes the gap, so a longer one never stretches the label
			if d := loaded.PrintableLengthDots - fallback.PrintableLengthDots; d <= 0 && float64(-d) <= math.Round(float64(fallback.DPI)/25.4) {
				fallback.PrintableLengthDots, fallback.LengthMM = loaded.PrintableLengthDots, loaded.LengthMM
			}
			return fallback, FromFallback, nil
		case loaded.ID == "unknown" && hasFallback:
			s.Log.Warn("printer reports media this agent doesn't know; using the configured media", "loaded", MediaName(loaded))
			return fallback, FromFallback, nil
		case loaded.ID == "unknown":
			return devicedb.Media{}, "", fmt.Errorf("printer has %s loaded, which this agent doesn't know: set `media:` for this printer", MediaName(loaded))
		}
		if allowed := p.AllowedMedia(model); !slices.Contains(allowed, loaded.ID) {
			return devicedb.Media{}, "", &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("printer has %s (%s) loaded; this printer takes %s", loaded.ID, loaded.Describe(), strings.Join(allowed, ", "))}
		}
		return *loaded, FromPrinter, nil
	}
	if k, ok := s.recentMedia(p, model); ok {
		return k, FromCache, nil
	}
	if hasFallback {
		return fallback, FromFallback, nil
	}
	return devicedb.Media{}, "", errors.New("media unknown: the printer doesn't report its roll; set `media:` for this printer as a fallback")
}

// recentMedia is the roll the printer reported in the last 30 s, if this printer may print on it.
func (s *Service) recentMedia(p *config.Printer, model *devicedb.Model) (devicedb.Media, bool) {
	m, at, ok := s.LastMedia(p.ID)
	return m, ok && time.Since(at) < mediaTTL && slices.Contains(p.AllowedMedia(model), m.ID)
}

func MediaName(m *devicedb.Media) string {
	if m == nil {
		return "none"
	}
	if m.ID == "unknown" {
		return fmt.Sprintf("unknown %s %gx%g mm", m.Kind, m.WidthMM, m.LengthMM)
	}
	return m.ID
}

// MediaStatus says which media a job would use after status st (the same choice as pickMedia, without changing anything): "dk-22214 (detected)" or
// "dk-22210 (config fallback)"; ok is false when jobs would be refused or fail. m is that
// media, nil if none.
func (s *Service) MediaStatus(p *config.Printer, st *driver.Status) (m *devicedb.Media, text string, ok bool) {
	var loaded *devicedb.Media
	if st != nil {
		loaded = st.Media
	}
	model, err := p.DeviceModel()
	if err != nil {
		return nil, err.Error(), false
	}
	if loaded != nil && loaded.ID != "unknown" {
		if !slices.Contains(p.AllowedMedia(model), loaded.ID) {
			return loaded, loaded.ID + " (detected; not in media_allowed: jobs will be refused)", false
		}
		return loaded, loaded.ID + " (detected)", true
	}
	if loaded == nil { // like pickMedia: a roll seen lately stands in for a status that names none
		if r, ok := s.recentMedia(p, model); ok {
			return &r, r.ID + " (" + FromCache + ")", true
		}
	}
	if f := p.FallbackMedia(); f != "" {
		if fm, ok := LookupMedia(p, f); ok {
			return &fm, f + " (config fallback)", true
		}
	}
	return nil, MediaName(loaded) + " reported; jobs will fail: set media for this printer as a fallback", false
}

// FittingProfiles lists the label profiles of the config's Part-DB instance whose labels fit m,
// as "fits profiles: 2 part bag 29 mm, 4 drawer"; "" without an instance or on errors (logged).
func (s *Service) FittingProfiles(ctx context.Context, cfg *config.Config, m *devicedb.Media) string {
	in, err := cfg.Instance("")
	if err != nil || m == nil {
		return ""
	}
	ps, err := s.ProfilesFitting(ctx, partdb.FromInstance(in), m)
	if err != nil {
		s.Log.Warn("checking label profiles against the roll", "err", err)
	}
	if len(ps) == 0 {
		if err != nil {
			return ""
		}
		return "no Part-DB label profile fits " + m.Describe()
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = fmt.Sprintf("%d %s", p.ID, p.Name)
	}
	return "fits profiles: " + strings.Join(names, ", ")
}
