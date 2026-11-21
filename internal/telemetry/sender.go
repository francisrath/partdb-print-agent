package telemetry

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/update"
)

// Off says why no report is sent ("" = reports are on): an environment opt-out, a config that turned
// them off, or no choice made yet (the setup wizard or the web UI asks; until then nothing is sent).
func Off(cfg *config.Config) string {
	switch {
	case os.Getenv("DO_NOT_TRACK") != "" && os.Getenv("DO_NOT_TRACK") != "0":
		return "DO_NOT_TRACK is set"
	case strings.EqualFold(os.Getenv("PDB_PRINT_TELEMETRY"), "off"):
		return "PDB_PRINT_TELEMETRY=off"
	case cfg.Telemetry == nil:
		return "not decided yet (the setup wizard or the web UI asks)"
	case !*cfg.Telemetry:
		return "telemetry: false in the config"
	}
	return ""
}

// errRejected: the report is invalid or the collector refused it; retrying won't help.
var errRejected = errors.New("report rejected")

type Sender struct {
	Store    *Store
	Version  string
	Config   func() *config.Config
	Renderer func() string // pdfium | pdftoppm
	AgentAPI func() bool   // a connected Part-DB has the print agent API
	Endpoint string        // "" = Endpoint
	Client   *http.Client  // nil = 15 s timeout
	Log      *slog.Logger  // nil = none
}

// Build is the report the next send would carry (pdb-print telemetry show prints it).
func (s *Sender) Build() Report {
	c, _ := s.Store.due()
	cfg := s.Config()
	r := Report{
		Schema: Schema, ID: c.ID, Week: c.Since, Version: strings.TrimPrefix(s.Version, "v"), OS: runtime.GOOS, Arch: runtime.GOARCH,
		OSVersion: osVersion(), Board: board(), Install: update.Install(),
		Models: map[string]int{}, Transports: map[string]int{}, Jobs: c.Jobs, Labels: c.Labels, Errors: c.Errors,
	}
	r.Renderer = "unknown"
	if s.Renderer != nil {
		r.Renderer = cmp.Or(clip(s.Renderer()), "unknown")
	}
	if s.AgentAPI != nil {
		r.AgentAPI = s.AgentAPI()
	}
	db := devicedb.Default()
	for _, p := range cfg.Printers {
		if _, ok := db.Model(p.Model); ok {
			r.Models[p.Model]++
		} else {
			r.ModelsUnknown++
		}
		r.Transports[p.Transport.Kind]++
	}
	return r
}

// Run checks every hour whether a report is due and sends it when reports are on. Weeks counted while
// they are off are dropped, never sent later. Release builds only.
func (s *Sender) Run(ctx context.Context) {
	if !update.Released(s.Version) {
		return
	}
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// tick sends the report if one is due and reports are on.
func (s *Sender) tick(ctx context.Context) {
	if _, due := s.Store.due(); !due {
		return
	}
	if Off(s.Config()) != "" {
		_ = s.Store.reset(false)
		return
	}
	if err := s.Store.persistable(); err != nil {
		if s.Log != nil {
			s.Log.Debug("usage report not sent: can't save its state, so it might be sent twice", "err", err)
		}
		return
	}
	switch err := s.send(ctx); {
	case errors.Is(err, errRejected):
		_ = s.Store.reset(false) // the collector won't take it: don't retry every hour
	case err != nil:
		if s.Log != nil && ctx.Err() == nil {
			s.Log.Debug("usage report not sent; next try in an hour", "err", err)
		}
	default:
		_ = s.Store.reset(true)
	}
}

func (s *Sender) send(ctx context.Context) error {
	r := s.Build()
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	body, _ := json.Marshal(r)
	url, client := s.Endpoint, s.Client
	if url == "" {
		url = Endpoint
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pdb-print")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusRequestEntityTooLarge,
		resp.StatusCode == http.StatusUnprocessableEntity: // the report itself is refused; anything else is retried
		return fmt.Errorf("%w: HTTP %d", errRejected, resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
