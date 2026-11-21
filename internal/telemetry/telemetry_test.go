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

	"github.com/francisrath/partdb-print-agent/internal/config"
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

func TestDevBuildsNeverSend(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent++ }))
	defer srv.Close()
	now := mon
	s := sender(t, &config.Config{Telemetry: on()}, srv.URL, &now)
	s.Version = "dev"
	s.Store.Job("bookmarklet", "QL-810W", 1, "")
	now = next
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if sent != 0 {
		t.Error("a dev build sent a report")
	}
}

func TestValidate(t *testing.T) {
	good := Report{Schema: 1, Week: "2026-W47", Version: "0.3.0", OS: "linux", Arch: "arm", Install: "deb", Renderer: "pdftoppm",
		OSVersion: "debian 13", Board: "Raspberry Pi 2 Model B", Models: map[string]int{"QL-810W": 1}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(r *Report){
		"schema":        func(r *Report) { r.Schema = 2 },
		"week":          func(r *Report) { r.Week = "2026-47" },
		"dev version":   func(r *Report) { r.Version = "dev" },
		"free text key": func(r *Report) { r.Models = map[string]int{"my printer in Bob's office": 1} },
		"email":         func(r *Report) { r.Board = "bob@example.org" },
		"negative":      func(r *Report) { r.Labels = map[string]int{"QL-810W": -1} },
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
	_ = os.WriteFile(p, []byte("PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n"), 0o600)
	if got := linuxRelease(p); got != "debian 13" {
		t.Errorf("%q", got)
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
