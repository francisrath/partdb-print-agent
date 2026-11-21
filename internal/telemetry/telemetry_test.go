package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
)

var (
	mon  = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC) // 2026-W47
	next = mon.AddDate(0, 0, 7)                          // 2026-W48
)

func on() *bool  { b := true; return &b }
func off() *bool { b := false; return &b }

func TestWeek(t *testing.T) {
	for d, want := range map[time.Time]string{mon: "2026-W47", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC): "2026-W53"} {
		if got := Week(d); got != want {
			t.Errorf("%v: %s, want %s", d, got, want)
		}
	}
}

func TestOff(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("PDB_PRINT_TELEMETRY", "")
	for _, c := range []struct {
		cfg  config.Config
		env  [2]string
		want string // substring; "" = on
	}{
		{config.Config{}, [2]string{}, "not decided"},
		{config.Config{Telemetry: off()}, [2]string{}, "false"},
		{config.Config{Telemetry: on()}, [2]string{}, ""},
		{config.Config{Telemetry: on()}, [2]string{"DO_NOT_TRACK", "1"}, "DO_NOT_TRACK"},
		{config.Config{Telemetry: on()}, [2]string{"DO_NOT_TRACK", "0"}, ""},
		{config.Config{Telemetry: on()}, [2]string{"PDB_PRINT_TELEMETRY", "OFF"}, "PDB_PRINT_TELEMETRY"},
	} {
		if c.env[0] != "" {
			t.Setenv(c.env[0], c.env[1])
		}
		got := Off(&c.cfg)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v %v: %q, want %q", c.cfg.Telemetry, c.env, got, c.want)
		}
		if c.env[0] != "" {
			t.Setenv(c.env[0], "")
		}
	}
}

func TestStoreKeepsCountsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.json")
	s := &Store{Path: path, Now: func() time.Time { return mon }}
	s.Job("bookmarklet", "QL-810W", 3, "")
	s.Job("part-db", "QL-810W", 2, "")
	s.Job("part-db", "PT-9500PC", 0, "NO_MEDIA")
	s2 := Open(path)
	s2.Now = s.Now
	c, due := s2.due()
	if due || c.Since != "2026-W47" || c.Labels["QL-810W"] != 5 || c.Jobs["part-db"] != 2 || c.Errors["NO_MEDIA"] != 1 || c.Labels["PT-9500PC"] != 0 {
		t.Fatalf("%+v due=%v", c, due)
	}
	s2.Now = func() time.Time { return next }
	if _, due := s2.due(); !due {
		t.Error("not due in the next week")
	}
	s2.reset(true)
	if c, due := s2.due(); due || c.Since != "2026-W48" || c.Sent != "2026-W48" || len(c.Labels) != 0 {
		t.Errorf("after reset: %+v due=%v", c, due)
	}
}

func sender(t *testing.T, cfg *config.Config, url string, now *time.Time) *Sender {
	return &Sender{
		Store: &Store{Path: filepath.Join(t.TempDir(), "t.json"), Now: func() time.Time { return *now }}, Version: "0.3.0",
		Config: func() *config.Config { return cfg }, Renderer: func() string { return "pdfium" }, Endpoint: url,
	}
}

func TestSendOncePerWeek(t *testing.T) {
	var got []Report
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var rep Report
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rep); err != nil {
			t.Errorf("decode %s: %v", b, err)
		}
		got = append(got, rep)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	now := mon
	cfg := &config.Config{Telemetry: on(), Printers: []config.Printer{
		{ID: "ql", Model: "QL-810W", Transport: config.Transport{Kind: "tcp"}},
		{ID: "x", Model: "Nonexistent-9000", Transport: config.Transport{Kind: "usb"}},
	}}
	s := sender(t, cfg, srv.URL, &now)
	s.Store.Job("bookmarklet", "QL-810W", 4, "")
	ctx := context.Background()
	s.tick(ctx)
	if len(got) != 0 {
		t.Fatal("sent during the first week")
	}
	now = next
	s.tick(ctx)
	s.tick(ctx) // same week again: nothing more
	if len(got) != 1 {
		t.Fatalf("%d reports, want 1", len(got))
	}
	r := got[0]
	if r.Week != "2026-W47" || r.Version != "0.3.0" || r.Models["QL-810W"] != 1 || r.ModelsUnknown != 1 || r.Transports["usb"] != 1 ||
		r.Labels["QL-810W"] != 4 || r.Jobs["bookmarklet"] != 1 || r.Renderer != "pdfium" {
		t.Errorf("%+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
}

// Weeks counted while reports are off (or undecided) are dropped, not sent once they're turned on.
func TestOffWeeksAreDropped(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent++ }))
	defer srv.Close()
	now := mon
	cfg := &config.Config{}
	s := sender(t, cfg, srv.URL, &now)
	s.Store.Job("bookmarklet", "QL-810W", 4, "")
	ctx := context.Background()
	now = next
	s.tick(ctx) // undecided: dropped
	cfg.Telemetry = on()
	s.tick(ctx) // nothing due this week
	if sent != 0 {
		t.Fatalf("sent %d", sent)
	}
	if c, _ := s.Store.due(); c.Labels["QL-810W"] != 0 {
		t.Errorf("undecided week kept: %+v", c)
	}
}

// A dev build's Run returns at once: it never even touches the counters file (a tick would, before
// sending), whatever else would stop the send.
func TestDevBuildsNeverSend(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent++ }))
	defer srv.Close()
	now := mon
	s := sender(t, &config.Config{Telemetry: on()}, srv.URL, &now)
	s.Store.c.Since = "2026-W46" // a report is due
	s.Version = "dev"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run didn't return for a dev build")
	}
	if _, err := os.Stat(s.Store.Path); err == nil || sent != 0 {
		t.Errorf("dev build touched the store or sent (%d)", sent)
	}
}

func TestValidate(t *testing.T) {
	good := Report{Schema: 1, ID: "0123456789abcdef", Week: "2026-W47", Version: "0.3.0", OS: "linux", Arch: "arm", Install: "deb",
		Renderer: "pdftoppm", OSVersion: "debian 13", Board: "Raspberry Pi 2 Model B", Models: map[string]int{"QL-810W": 1},
		Transports: map[string]int{"usb": 1}, Jobs: map[string]int{"part-db": 3}, Labels: map[string]int{"QL-810W": 9},
		Errors: map[string]int{"NO_MEDIA": 1}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(r *Report){
		"schema":            func(r *Report) { r.Schema = 2 },
		"no id":             func(r *Report) { r.ID = "" },
		"week":              func(r *Report) { r.Week = "2026-47" },
		"dev version":       func(r *Report) { r.Version = "dev" },
		"plain words model": func(r *Report) { r.Models = map[string]int{"Bobs office printer": 1} },
		"host as model":     func(r *Report) { r.Labels = map[string]int{"bobs-pi.lan": 1} },
		"ip as transport":   func(r *Report) { r.Transports = map[string]int{"192.168.1.50": 1} },
		"job source":        func(r *Report) { r.Jobs = map[string]int{"visit spam.example.com": 1} },
		"error code":        func(r *Report) { r.Errors = map[string]int{"AA-BB-CC-DD-EE-FF": 1} },
		"board host":        func(r *Report) { r.Board = "bob.example.org" },
		"os_version text":   func(r *Report) { r.OSVersion = "bobs laptop" },
		"os":                func(r *Report) { r.OS = "plan9" },
		"install":           func(r *Report) { r.Install = "bobs-script" },
		"negative":          func(r *Report) { r.Labels = map[string]int{"QL-810W": -1} },
		"huge":              func(r *Report) { r.Labels = map[string]int{"QL-810W": 9e18} },
	} {
		r := good
		bad(&r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLinuxRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "os-release")
	for content, want := range map[string]string{
		"PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n": "debian 13",
		"ID=ubuntu\nVERSION_ID=\"24.04\"\n":                                            "ubuntu 24",
		"ID=bobs-own-distro\nVERSION_ID=1\n":                                           "linux", // not in the list: no free text
		"ID=arch\n":                                                                    "arch",
	} {
		_ = os.WriteFile(p, []byte(content), 0o600)
		if got := linuxRelease(p); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
}

// docs/telemetry.md lists every field the report has.
func TestDocListsEveryField(t *testing.T) {
	doc, err := os.ReadFile("../../docs/telemetry.md")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Report{})
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	for f := range fields {
		if !strings.Contains(string(doc), "`"+f+"`") {
			t.Errorf("docs/telemetry.md doesn't list `%s`", f)
		}
	}
}

// Reset (reports switched on) drops what was counted before, and gives the next report a new id.
func TestResetOnOptIn(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "t.json"), Now: func() time.Time { return mon }}
	s.Job("bookmarklet", "QL-810W", 200, "")
	c, _ := s.due()
	s.Reset()
	c2, _ := s.due()
	if len(c2.Labels) != 0 || c2.ID == c.ID || c2.ID == "" {
		t.Errorf("before %+v, after %+v", c, c2)
	}
}

// A report that can't be remembered as sent isn't sent (it would go out again after a restart).
func TestNoSendWithoutPersistence(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent++ }))
	defer srv.Close()
	now := next
	s := sender(t, &config.Config{Telemetry: on()}, srv.URL, &now)
	s.Store.c.Since = "2026-W47"
	s.Store.Path = filepath.Join(t.TempDir(), "missing-dir", "sub", "t.json")
	if err := os.WriteFile(filepath.Dir(filepath.Dir(s.Store.Path)), nil, 0o600); err != nil { // a file where the dir should be
		t.Fatal(err)
	}
	s.tick(context.Background())
	if sent != 0 {
		t.Error("sent although the store can't be saved")
	}
}

// Only a refusal of the report itself drops the week; a proxy's 429 or 403 is retried.
func TestRejectedVsRetried(t *testing.T) {
	for code, dropped := range map[int]bool{400: true, 413: true, 422: true, 429: false, 403: false, 503: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		now := next
		s := sender(t, &config.Config{Telemetry: on()}, srv.URL, &now)
		s.Store.c.Since = "2026-W47"
		s.Store.Job("bookmarklet", "QL-810W", 1, "")
		s.tick(context.Background())
		c, _ := s.Store.due()
		if gone := c.Since == "2026-W48"; gone != dropped {
			t.Errorf("HTTP %d: dropped %v, want %v", code, gone, dropped)
		}
		srv.Close()
	}
}
