// pdb-print-stats receives pdb-print's weekly usage reports (internal/telemetry, docs/telemetry.md) and
// appends each one, as received, to a JSON Lines file. It never reads or stores the sender's address,
// sets no cookies and logs no requests. Query the file with jq, DuckDB or sqlite.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/telemetry"
)

const (
	maxFile   = 256 << 20 // stop accepting before the disk fills (several hundred thousand reports)
	maxPerDay = 5000      // reports stored per UTC day: a curl loop can't flood the file
	maxAge    = 8         // weeks: older (or future) weeks are refused, so a forged week can't take over the page
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	path := flag.String("file", "/data/reports.jsonl", "JSON Lines file the reports are appended to")
	flag.Parse()
	c := &collector{path: *path, now: time.Now}
	if err := c.load(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.Info("pdb-print-stats", "addr", *addr, "file", *path, "reports", len(c.seen))
	srv := &http.Server{Addr: *addr, Handler: c.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8 << 10,
		ErrorLog: log.New(io.Discard, "", 0)} // net/http's own errors can name the client address
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type collector struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	seen  map[string]bool // report ids stored: a resent report counts once
	day   string          // UTC day of today's count
	today int             // reports stored today
	cache *page           // the stats page's numbers; nil after a new report
}

// load reads the ids already stored.
func (c *collector) load() error {
	c.seen = map[string]bool{}
	f, err := os.Open(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var s struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.ID != "" {
			c.seen[s.ID] = true
		}
	}
	return sc.Err()
}

// stored is one line of the file: the report as sent, plus the day it arrived.
type stored struct {
	Received string `json:"received"` // 2026-11-23 (UTC day, nothing finer)
	telemetry.Report
}

func (c *collector) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/report", c.report)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", c.stats)
	mux.HandleFunc("GET /v1/badge/installs.json", c.badge)
	return mux
}

func (c *collector) report(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, telemetry.MaxBytes))
	dec.DisallowUnknownFields()
	var rep telemetry.Report
	if err := dec.Decode(&rep); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := rep.Validate(); err != nil {
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := c.now().UTC()
	if start, ok := weekStart(rep.Week); !ok || start.After(now) || now.Sub(start) > maxAge*7*24*time.Hour {
		http.Error(w, "week out of range", http.StatusUnprocessableEntity)
		return
	}
	line, _ := json.Marshal(stored{Received: now.Format(time.DateOnly), Report: rep})
	switch err := c.append(rep.ID, now.Format(time.DateOnly), append(line, '\n')); {
	case errors.Is(err, errDuplicate):
	case errors.Is(err, errFull):
		http.Error(w, "not stored now", http.StatusServiceUnavailable)
		return
	case err != nil:
		slog.Error("storing a report", "err", err)
		http.Error(w, "not stored", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	errFull      = errors.New("no more reports taken today, or the file is full")
	errDuplicate = errors.New("report already stored")
)

func (c *collector) append(id, day string, line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if c.seen[id] {
		return errDuplicate
	}
	if c.day != day {
		c.day, c.today = day, 0
	}
	if c.today >= maxPerDay {
		return errFull
	}
	f, err := os.OpenFile(c.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > maxFile {
		return errFull
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	c.seen[id], c.cache = true, nil
	c.today++
	return nil
}
