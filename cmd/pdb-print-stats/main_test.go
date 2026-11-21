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

const good = `{"schema":1,"week":"2026-W47","version":"0.3.0","os":"linux","arch":"arm","os_version":"debian 13",
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
	c := &collector{path: path, now: func() time.Time { return time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC) }}
	h := c.handler()
	if code := post(t, h, good); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if code := post(t, h, good); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
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
	h := (&collector{path: path, now: time.Now}).handler()
	for name, body := range map[string]string{
		"not json":      "hello",
		"unknown field": strings.Replace(good, `"schema":1`, `"schema":1,"hostname":"bobs-pi"`, 1),
		"free text":     strings.Replace(good, `"PT-9500PC":1}`, `"Bob's printer <b>":1}`, 1),
		"too big":       strings.Replace(good, `"schema":1`, `"schema":1,"x":"`+strings.Repeat("a", 9000)+`"`, 1),
	} {
		if code := post(t, h, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, code)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a bad report was stored")
	}
}

func TestStatsPageAndBadge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.jsonl")
	h := (&collector{path: path, now: time.Now}).handler()
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
	post(t, h, good)
	post(t, h, strings.Replace(good, `"2026-W47"`, `"2026-W48"`, 1))
	post(t, h, strings.Replace(strings.Replace(good, `"2026-W47"`, `"2026-W48"`, 1), `"0.3.0"`, `"0.3.1"`, 1))
	body := get("/")
	for _, want := range []string{"<b>2</b>installs in 2026-W48", "<b>90</b>labels printed", "0.3.1", "Raspberry Pi 2 Model B", "PT-9500PC", "NO_MEDIA"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	var badge map[string]any
	if err := json.Unmarshal([]byte(get("/v1/badge/installs.json")), &badge); err != nil || badge["message"] != "2" || badge["schemaVersion"] != float64(1) {
		t.Errorf("badge %v %v", badge, err)
	}
}
