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
