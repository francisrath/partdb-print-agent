package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// now is in 2026-W48; good reports W47.
var now = func() time.Time { return time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC) }

const good = `{"schema":1,"id":"0123456789abcdef","week":"2026-W47","version":"0.3.0","os":"linux","arch":"arm","os_version":"debian 13",
"board":"Raspberry Pi 2 Model B","install":"deb","renderer":"pdftoppm","agent_api":true,"models":{"PT-9500PC":1},
"models_unknown":0,"transports":{"usb":1},"jobs":{"part-db":12},"labels":{"PT-9500PC":30},"errors":{"NO_MEDIA":1}}`

func post(t *testing.T, h http.Handler, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/report", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:4242"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestReportIsAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	c := &collector{path: path, now: now}
	h := c.handler()
	if code := post(t, h, good); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if code := post(t, h, good); code != http.StatusNoContent { // resent (lost reply): accepted, stored once
		t.Fatalf("status %d", code)
	}
	if code := post(t, h, strings.Replace(good, "0123456789abcdef", "fedcba9876543210", 1)); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 { // the resent report is stored once
		t.Fatalf("%d lines", len(lines))
	}
	var s stored
	if err := json.Unmarshal([]byte(lines[0]), &s); err != nil || s.Received != "2026-11-23" || s.Labels["PT-9500PC"] != 30 || !s.AgentAPI {
		t.Errorf("%+v %v", s, err)
	}
	if strings.Contains(string(b), "203.0.113.7") {
		t.Error("the sender's address was stored")
	}
}

func TestBadReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	h := (&collector{path: path, now: now}).handler()
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"not json":      {"hello", http.StatusBadRequest},
		"unknown field": {strings.Replace(good, `"schema":1`, `"schema":1,"hostname":"bobs-pi"`, 1), http.StatusBadRequest},
		"free text":     {strings.Replace(good, `"PT-9500PC":1}`, `"Bobs office printer":1}`, 1), http.StatusBadRequest},
		"too big":       {strings.Replace(good, `"jobs":{`, `"jobs":{`+strings.Repeat(" ", 9000), 1), http.StatusRequestEntityTooLarge},
		"future week":   {strings.Replace(good, "2026-W47", "9999-W52", 1), http.StatusUnprocessableEntity},
		"old week":      {strings.Replace(good, "2026-W47", "2026-W30", 1), http.StatusUnprocessableEntity},
		"no such week":  {strings.Replace(good, "2026-W47", "2026-W53", 1), http.StatusUnprocessableEntity},
	} {
		if code := post(t, h, c.body); code != c.code {
			t.Errorf("%s: status %d, want %d", name, code, c.code)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a bad report was stored")
	}
}

func TestStatsPageAndBadge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	h := (&collector{path: path, now: now}).handler()
	get := func(p string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", p, w.Code)
		}
		return w.Body.String()
	}
	if body := get("/"); !strings.Contains(body, "No reports yet") {
		t.Errorf("empty page: %s", body)
	}
	w48 := func(id, ver string) string {
		return strings.Replace(strings.Replace(strings.Replace(good, "2026-W47", "2026-W48", 1), "0123456789abcdef", id, 1), "0.3.0", ver, 1)
	}
	post(t, h, strings.Replace(good, "2026-W47", "2026-W45", 1)) // W46 has none: shown as 0
	post(t, h, w48("1111111111111111", "0.3.0"))
	post(t, h, w48("2222222222222222", "0.3.1"))
	body := get("/")
	for _, want := range []string{"<b>2</b>installs in 2026-W48", "<b>90</b>labels printed", `title="2026-W46: 0"`, "0.3.1", "Raspberry Pi 2 Model B", "PT-9500PC", "NO_MEDIA"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	var badge map[string]any
	if err := json.Unmarshal([]byte(get("/v1/badge/installs.json")), &badge); err != nil || badge["message"] != "2" || badge["schemaVersion"] != float64(1) {
		t.Errorf("badge %v %v", badge, err)
	}
}

func TestDailyCap(t *testing.T) {
	c := &collector{path: filepath.Join(t.TempDir(), "reports.jsonl"), now: now, day: "2026-11-23", today: maxPerDay}
	if code := post(t, c.handler(), good); code != http.StatusServiceUnavailable {
		t.Errorf("over the daily cap: status %d", code)
	}
}

func TestLoadRemembersIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	post(t, (&collector{path: path, now: now}).handler(), good)
	c := &collector{path: path, now: now}
	if err := c.load(); err != nil || !c.seen["0123456789abcdef"] {
		t.Fatalf("%v %v", c.seen, err)
	}
	post(t, c.handler(), good) // after a restart, still stored once
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Errorf("%d lines", n)
	}
}
