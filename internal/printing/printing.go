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
	"strings"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherql"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
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
		WidthDots:     m.PrintableWidthDots,
		LengthDots:    m.PrintableLengthDots,
		MediaWidthMM:  m.WidthMM,
		MediaLengthMM: m.LengthMM,
		CheckSize:     !p.AllowScaling,
		Rotate:        p.Rotate,
		Threshold:     p.Threshold,
	})
}

// Render rasterises pdf for media without printing (previews).
func (s *Service) Render(pdf []byte, p *config.Printer, m *driver.Media) ([]*image.Gray, error) {
	return s.render(pdf, p, m)
}

func addr(p *config.Printer) string {
	return net.JoinHostPort(p.Transport.Host, strconv.Itoa(p.Transport.Port))
}

// Status queries the printer: SNMP first, then the driver's status over TCP.
// driver.ErrNoReadback if neither answers.
func Status(ctx context.Context, p *config.Printer) (*driver.Status, error) {
	if ss, err := querySNMP(ctx, p); err == nil {
		return statusFromSNMP(ss), nil
	}
	t := tcp.New(addr(p))
	if err := t.Open(ctx); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	defer t.Close()
	return brotherql.New().Status(ctx, t)
}

func querySNMP(ctx context.Context, p *config.Printer) (*snmp.State, error) {
	if p.NoSNMP {
		return nil, errors.New("snmp disabled")
	}
	community := p.SNMPCommunity
	if community == "" {
		community = "public"
	}
	return snmp.Query(ctx, p.Transport.Host, community, 2*time.Second)
}

func statusFromSNMP(ss *snmp.State) *driver.Status {
	st := &driver.Status{Code: driver.OK, Detail: ss.Display, Model: strings.TrimPrefix(ss.Model, "Brother ")}
	if prob := ss.Problem(); prob != nil {
		st.Code, st.Detail = prob.Code, prob.Detail
	}
	if kind, w, l, ok := ss.LoadedMedia(); ok {
		if m, ok := brotherql.MediaFor(kind, w, l); ok {
			st.Media = &m
		} else {
			st.Media = &driver.Media{ID: "unknown", Kind: kind, WidthMM: float64(w), LengthMM: float64(l)}
		}
	}
	return st
}

// Print checks the printer (SNMP, else TCP status), settles the media (config or what the
// printer reports), gets the PDF from source, renders, encodes and sends it, then waits for
// the printer to confirm. With dryRun set, the byte stream goes to that file instead.
func (s *Service) Print(ctx context.Context, p *config.Printer, dryRun string, source Source) (*Result, error) {
	lock := s.printerLock(p.ID)
	lock.Lock()
	defer lock.Unlock()

	drv := brotherql.New()
	if _, err := drv.Capabilities(p.Model); err != nil {
		return nil, err
	}
	var t *tcp.Transport
	readback, viaSNMP := false, false
	media, ok := brotherql.LookupMedia(p.Media)
	if p.Media != "" && !ok {
		return nil, fmt.Errorf("unknown media %q", p.Media)
	}
	if dryRun == "" {
		var st *driver.Status
		if ss, err := querySNMP(ctx, p); err == nil {
			st, viaSNMP = statusFromSNMP(ss), true
		} else {
			s.Log.Debug("no SNMP status", "printer", p.ID, "err", err)
		}
		t = tcp.New(addr(p))
		if err := t.Open(ctx); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		defer t.Close()
		if !viaSNMP {
			var err error
			st, err = drv.Status(ctx, t)
			switch {
			case errors.Is(err, driver.ErrNoReadback):
				s.Log.Warn("printer sent no status; printing blind", "printer", p.ID)
			case err != nil:
				return nil, err
			default:
				readback = true
			}
		}
		if st != nil {
			s.Log.Info("printer status", "model", st.Model, "code", st.Code, "media", MediaName(st.Media), "via", map[bool]string{true: "snmp", false: "tcp"}[viaSNMP])
			if st.Code != driver.OK {
				return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
			}
			switch {
			case st.Media == nil:
			case st.Media.ID == "unknown":
				s.Log.Warn("printer reports a roll this agent doesn't know; using the configured media", "loaded", MediaName(st.Media))
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
	pages, err := s.render(pdf, p, &media)
	if errors.Is(err, render.ErrSize) {
		return nil, &driver.Error{Code: driver.MediaMismatch, Detail: err.Error() + " (set allow_scaling to print it scaled)"}
	}
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
	timeout := 30*time.Second + time.Duration(len(pages))*5*time.Second
	msg := "sent (unconfirmed: printer gives no status)"
	switch {
	case viaSNMP:
		done, err := s.waitSNMP(ctx, p, timeout)
		if err != nil {
			return nil, err
		}
		if done {
			msg, res.Confirmed = "printed", true
		} else {
			msg = "sent (unconfirmed: printer didn't report printing)"
		}
	case readback:
		err := brotherql.WaitDone(ctx, t, len(pages), timeout)
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

// waitSNMP polls until the printer has printed and is idle again (done), reports an
// error, or the timeout passes (not done). Observed on a QL-810W: idle → printing →
// other ("BUSY", cutting) → idle; errors show as device down plus a Printer-MIB alert.
// ponytail: "idle for 1 s after activity" = done; a printer that pauses longer between
// pages of a big job would be reported done early.
func (s *Service) waitSNMP(ctx context.Context, p *config.Printer, timeout time.Duration) (bool, error) {
	const poll = 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	active, idle := false, 0
	for first := true; time.Now().Before(deadline); first = false {
		wait := poll
		if first {
			wait = 0 // a short job can finish within one poll interval
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
		ss, err := querySNMP(ctx, p)
		if err != nil {
			continue
		}
		if prob := ss.Problem(); prob != nil {
			return false, prob
		}
		switch {
		case ss.Printing:
			active, idle = true, 0
		case ss.Idle && active:
			if idle++; idle >= 4 {
				return true, nil
			}
		}
	}
	return false, nil
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
