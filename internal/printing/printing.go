// Package printing runs a print job end to end: printer status → media → PDF → raster → driver → transport.
// It is shared by the CLI and the local server.
package printing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherql"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
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
		return pdfgen.TestCard(m.WidthMM, length, []string{
			"pdb-print test", model, m.ID, fmt.Sprintf("%d dots", m.PrintableWidthDots), version,
		}), nil
	}
}

type Result struct {
	Pages     int
	Media     string
	Bytes     int
	Confirmed bool // the printer reported completion
}

// Service keeps one PDFium renderer warm and serialises jobs per printer.
type Service struct {
	Log *slog.Logger

	mu       sync.Mutex // guards r and printers
	r        *render.Renderer
	printers map[string]*sync.Mutex
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r != nil {
		_ = s.r.Close()
		s.r = nil
	}
}

func (s *Service) printerLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.printers == nil {
		s.printers = map[string]*sync.Mutex{}
	}
	if s.printers[id] == nil {
		s.printers[id] = &sync.Mutex{}
	}
	return s.printers[id]
}

func (s *Service) render(pdf []byte, p *config.Printer, m *driver.Media) ([]*image.Gray, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.r == nil {
		r, err := render.New()
		if err != nil {
			return nil, err
		}
		s.r = r
	}
	return s.r.Render(pdf, render.Options{
		WidthDots:    m.PrintableWidthDots,
		LengthDots:   m.PrintableLengthDots,
		MediaWidthMM: m.WidthMM,
		Rotate:       p.Rotate,
		Threshold:    p.Threshold,
	})
}

// Render rasterises pdf for media without printing (previews).
func (s *Service) Render(pdf []byte, p *config.Printer, m *driver.Media) ([]*image.Gray, error) {
	return s.render(pdf, p, m)
}

func addr(p *config.Printer) string {
	return net.JoinHostPort(p.Transport.Host, strconv.Itoa(p.Transport.Port))
}

// Status queries the printer; driver.ErrNoReadback if it doesn't answer.
func Status(ctx context.Context, p *config.Printer) (*driver.Status, error) {
	t := tcp.New(addr(p))
	if err := t.Open(ctx); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	defer t.Close()
	return brotherql.New().Status(ctx, t)
}

// Print settles the media (config or what the printer reports), gets the PDF from source,
// renders, encodes and sends it. With dryRun set, the byte stream goes to that file instead.
func (s *Service) Print(ctx context.Context, p *config.Printer, dryRun string, source Source) (*Result, error) {
	lock := s.printerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	drv := brotherql.New()
	if _, err := drv.Capabilities(p.Model); err != nil {
		return nil, err
	}
	var t *tcp.Transport
	readback := false
	media, ok := brotherql.LookupMedia(p.Media)
	if p.Media != "" && !ok {
		return nil, fmt.Errorf("unknown media %q", p.Media)
	}
	if dryRun == "" {
		t = tcp.New(addr(p))
		if err := t.Open(ctx); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		defer t.Close()
		st, err := drv.Status(ctx, t)
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			s.Log.Warn("printer sent no status; printing blind", "printer", p.ID)
		case err != nil:
			return nil, err
		default:
			readback = true
			s.Log.Info("printer status", "model", st.Model, "code", st.Code, "media", MediaName(st.Media))
			if st.Code != driver.OK {
				return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
			}
			if st.Media != nil && p.Media != "" && st.Media.ID != p.Media {
				return nil, &driver.Error{Code: driver.MediaMismatch,
					Detail: fmt.Sprintf("config says %s, printer has %s loaded", p.Media, MediaName(st.Media))}
			}
			if p.Media == "" && st.Media != nil {
				media, ok = brotherql.LookupMedia(st.Media.ID)
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
	pages, err := s.render(pdf, p, &media)
	if err != nil {
		return nil, &driver.Error{Code: driver.RenderError, Detail: err.Error()}
	}
	s.Log.Info("rendered", "pages", len(pages), "media", media.ID, "took", time.Since(start).Round(time.Millisecond))

	var job bytes.Buffer
	if err := drv.Encode(ctx, &job, pages, &driver.Job{Cut: p.Cut}, &media); err != nil {
		return nil, err
	}
	res := &Result{Pages: len(pages), Media: media.ID, Bytes: job.Len()}
	if dryRun != "" {
		return res, os.WriteFile(dryRun, job.Bytes(), 0o644)
	}
	if _, err := t.Write(job.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	msg := "sent (unconfirmed: printer gives no status)"
	if readback {
		err := brotherql.WaitDone(ctx, t, len(pages), 30*time.Second+time.Duration(len(pages))*5*time.Second)
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			msg = "sent (unconfirmed: no completion status received)"
		case err != nil:
			return nil, err
		default:
			msg, res.Confirmed = "printed", true
		}
	}
	s.Log.Info(msg, "printer", p.ID, "pages", len(pages), "bytes", job.Len(), "took", time.Since(start).Round(time.Millisecond))
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
