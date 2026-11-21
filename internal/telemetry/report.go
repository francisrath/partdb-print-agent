// Package telemetry is pdb-print's weekly usage report: what it contains, the local counters it is
// built from, and the sender. docs/telemetry.md lists every field; TestDocListsEveryField keeps them
// in step. The collector (cmd/pdb-print-stats) decodes the same Report and checks it with Validate.
package telemetry

import (
	"errors"
	"fmt"
	"regexp"
)

// Schema is bumped on any change to Report.
const Schema = 1

// Endpoint receives the reports (cmd/pdb-print-stats on the maintainer's server). It never changes:
// released agents have it compiled in.
const Endpoint = "https://pdb-print-stats.cloudline.no/v1/report"

// MaxBytes is the largest report the collector accepts.
const MaxBytes = 8 << 10

// Report is sent at most once per ISO week. It holds no address, name, serial number, URL, label
// content or config value: only the fields below.
type Report struct {
	Schema    int    `json:"schema"`
	Week      string `json:"week"`       // ISO week the counts cover: 2026-W47
	Version   string `json:"version"`    // pdb-print release: 0.3.0
	OS        string `json:"os"`         // linux | darwin | windows
	Arch      string `json:"arch"`       // amd64 | arm64 | arm
	OSVersion string `json:"os_version"` // coarse: "debian 13", "macos 15", "windows 11"
	Board     string `json:"board"`      // "Raspberry Pi 2 Model B", else ""
	Install   string `json:"install"`    // deb | brew | docker | zip | archive
	Renderer  string `json:"renderer"`   // pdfium | pdftoppm
	// AgentAPI: a connected Part-DB has the print agent API (print jobs from Part-DB).
	AgentAPI bool `json:"agent_api"`
	// Models and Transports count the configured printers by device-database model and by
	// connection kind; ModelsUnknown counts printers whose model isn't in the database.
	Models        map[string]int `json:"models"`
	ModelsUnknown int            `json:"models_unknown"`
	Transports    map[string]int `json:"transports"`
	// Jobs counts print jobs per source (bookmarklet, part-db, hot-folder, web-ui, setup), Labels
	// the pages printed per model, Errors failed jobs per error code; all since the last report.
	Jobs   map[string]int `json:"jobs"`
	Labels map[string]int `json:"labels"`
	Errors map[string]int `json:"errors"`
}

var (
	weekRe    = regexp.MustCompile(`^\d{4}-W\d{2}$`)
	versionRe = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,4}$`)
	wordRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+-]{0,63}$`) // model ids, codes, OS names
)

// Validate is what the collector accepts: schema 1, sane sizes, only word-like keys and values (no free
// text, so nothing personal can be smuggled in a field).
func (r *Report) Validate() error {
	if r.Schema != Schema {
		return fmt.Errorf("schema %d, want %d", r.Schema, Schema)
	}
	if !weekRe.MatchString(r.Week) {
		return fmt.Errorf("week %q", r.Week)
	}
	if !versionRe.MatchString(r.Version) {
		return fmt.Errorf("version %q", r.Version)
	}
	for name, v := range map[string]string{"os": r.OS, "arch": r.Arch, "install": r.Install, "renderer": r.Renderer} {
		if !wordRe.MatchString(v) {
			return fmt.Errorf("%s %q", name, v)
		}
	}
	for name, v := range map[string]string{"os_version": r.OSVersion, "board": r.Board} {
		if v != "" && !wordRe.MatchString(v) {
			return fmt.Errorf("%s %q", name, v)
		}
	}
	if r.ModelsUnknown < 0 {
		return errors.New("models_unknown < 0")
	}
	for name, m := range map[string]map[string]int{"models": r.Models, "transports": r.Transports, "jobs": r.Jobs, "labels": r.Labels, "errors": r.Errors} {
		if len(m) > 64 {
			return fmt.Errorf("%s: %d keys", name, len(m))
		}
		for k, n := range m {
			if !wordRe.MatchString(k) || n < 0 {
				return fmt.Errorf("%s: %q = %d", name, k, n)
			}
		}
	}
	return nil
}
