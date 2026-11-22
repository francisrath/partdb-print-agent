// Package telemetry is pdb-print's weekly usage report: what it contains, the local counters it is
// built from, and the sender. docs/telemetry.md lists every field; TestDocListsEveryField keeps them
// in step. The collector (cmd/pdb-print-stats) decodes the same Report and checks it with Validate.
package telemetry

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/francisrath/pdb-print/internal/devicedb"
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
	ID        string `json:"id"`         // random, new every week: a resent report counts once
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
	idRe      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// os_version: an allowed distribution with its major version, "macos 15", or "windows 11 build 26100"
	osVersionRe = regexp.MustCompile(`^(?:(?:[a-z-]+)(?: \d{1,3})?|windows (?:10|11|server) build \d{4,6})$`)
	boardRe     = regexp.MustCompile(`^Raspberry Pi [0-9A-Za-z][0-9A-Za-z +.]{0,30}$`)
)

// maxCount caps every count: a week of a busy print server is far below it.
const maxCount = 1_000_000

// The closed sets of values a report may carry. Anything else is rejected, so a forged report can only
// repeat known words, never put free text (a host name, an address) on the public stats page.
var (
	oses       = set("linux", "darwin", "windows", "freebsd", "netbsd", "openbsd")
	arches     = set("amd64", "arm64", "arm", "386", "riscv64", "ppc64le", "s390x", "loong64")
	installs   = set("deb", "brew", "docker", "windows", "zip", "archive")
	renderers  = set("pdfium", "pdftoppm", "unknown")
	transports = set("tcp", "usb", "ble", "bt", "serial")
	jobSources = set("bookmarklet", "part-db", "hot-folder", "web-ui", "setup", "other")
	errorCodes = set("BUSY", "NO_MEDIA", "MEDIA_MISMATCH", "MEDIA_END_SOON", "COVER_OPEN", "CUTTER_JAM", "PAPER_JAM",
		"OVERHEAT", "PAUSED", "RIBBON_OUT", "COMMS_ERROR", "UNSUPPORTED", "RENDER_ERROR", "UNKNOWN", "OTHER")
	// distros: os-release IDs reported by name; others are reported as "linux".
	distros = set("debian", "ubuntu", "raspbian", "fedora", "centos", "rhel", "rocky", "almalinux", "arch", "manjaro",
		"alpine", "opensuse-leap", "opensuse-tumbleweed", "linuxmint", "pop", "nixos", "gentoo", "void", "elementary",
		"zorin", "kali", "linux", "macos", "windows")
)

func set(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Validate is what the collector accepts (and the agent checks before sending): schema 1, known values
// only, models from the device database, counts up to maxCount.
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
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("id %q", r.ID)
	}
	for name, c := range map[string]struct {
		v  string
		ok map[string]bool
	}{"os": {r.OS, oses}, "arch": {r.Arch, arches}, "install": {r.Install, installs}, "renderer": {r.Renderer, renderers}} {
		if !c.ok[c.v] {
			return fmt.Errorf("%s %q", name, c.v)
		}
	}
	if r.OSVersion != "" {
		name, _, _ := strings.Cut(r.OSVersion, " ")
		if !osVersionRe.MatchString(r.OSVersion) || !distros[name] {
			return fmt.Errorf("os_version %q", r.OSVersion)
		}
	}
	if r.Board != "" && !boardRe.MatchString(r.Board) {
		return fmt.Errorf("board %q", r.Board)
	}
	if r.ModelsUnknown < 0 || r.ModelsUnknown > maxCount {
		return fmt.Errorf("models_unknown %d", r.ModelsUnknown)
	}
	db := devicedb.Default()
	model := func(k string) bool { _, ok := db.Model(k); return ok }
	for name, c := range map[string]struct {
		m  map[string]int
		ok func(string) bool
	}{
		"models": {r.Models, model}, "labels": {r.Labels, model},
		"transports": {r.Transports, func(k string) bool { return transports[k] }},
		"jobs":       {r.Jobs, func(k string) bool { return jobSources[k] }},
		"errors":     {r.Errors, func(k string) bool { return errorCodes[k] }},
	} {
		for k, n := range c.m {
			if !c.ok(k) || n < 0 || n > maxCount {
				return fmt.Errorf("%s: %q = %d", name, k, n)
			}
		}
	}
	return nil
}
