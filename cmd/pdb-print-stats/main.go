// pdb-print-stats receives pdb-print's weekly usage reports (internal/telemetry, docs/telemetry.md) and
// appends each one, as received, to a JSON Lines file. It never reads or stores the sender's address,
// sets no cookies and logs no requests. Query the file with jq, DuckDB or sqlite.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/telemetry"
)

// maxFile stops accepting reports before the disk fills (about two million reports).
const maxFile = 1 << 30

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	path := flag.String("file", "/data/reports.jsonl", "JSON Lines file the reports are appended to")
	flag.Parse()
	c := &collector{path: *path, now: time.Now}
	slog.Info("pdb-print-stats", "addr", *addr, "file", *path)
	srv := &http.Server{Addr: *addr, Handler: c.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type collector struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pdb-print usage statistics collector: https://github.com/francisrath/partdb-print-agent/blob/main/docs/telemetry.md\n"))
	})
	return mux
}

func (c *collector) report(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, telemetry.MaxBytes))
	dec.DisallowUnknownFields()
	var rep telemetry.Report
	if err := dec.Decode(&rep); err != nil {
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := rep.Validate(); err != nil {
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	line, _ := json.Marshal(stored{Received: c.now().UTC().Format(time.DateOnly), Report: rep})
	if err := c.append(append(line, '\n')); err != nil {
		slog.Error("storing a report", "err", err)
		http.Error(w, "not stored", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errFull = errors.New("report file is full")

func (c *collector) append(line []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	return f.Sync()
}
