package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/zalando/go-keyring"
)

// fakeQL accepts print jobs on localhost and counts the ones that end with "print with feed".
func fakeQL(t *testing.T) (addr string, jobs chan int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	jobs = make(chan int, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c) // never answers status: like the QL-810W over TCP
				if len(b) > 0 && b[len(b)-1] == 0x1a {
					jobs <- len(b)
				}
			}()
		}
	}()
	return ln.Addr().String(), jobs
}

func fakePartDB(t *testing.T) *httptest.Server {
	pdf, err := os.ReadFile("../../testdata/sample-29x46.pdf")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tcp_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/tokens/current":
			fmt.Fprint(w, `{"name":"agent","user":{"name":"fran"},"level":1}`)
		case "/api/label_profiles":
			if r.URL.Query().Get("options.supported_element") == "part" {
				fmt.Fprint(w, `{"hydra:member":[{"@id":"/api/label_profiles/2","name":"bag 29 mm"}]}`)
			} else {
				fmt.Fprint(w, `{"hydra:member":[]}`)
			}
		case "/api/labels/generate":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(pdf)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	host   string
	jobs   chan int
	partdb *httptest.Server
}

func newHarness(t *testing.T) *harness {
	keyring.MockInit()
	t.Setenv("PDB_PRINT_TOKEN", "")
	t.Setenv("PDB_PRINT_URL", "")
	qlAddr, jobs := fakeQL(t)
	qlHost, qlPort, _ := net.SplitHostPort(qlAddr)
	port, _ := strconv.Atoi(qlPort)
	cfg := &config.Config{Printers: []config.Printer{{ID: "ql", Driver: "brother-ql", Model: "QL-810W", Media: "dk-22210", NoSNMP: true,
		Transport: config.Transport{Kind: "tcp", Host: qlHost, Port: port}}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &printing.Service{Log: log}
	t.Cleanup(svc.Close)
	s := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), svc, log, "127.0.0.1:8631", "test")
	return &harness{t: t, srv: s, h: s.Handler(), host: "127.0.0.1:8631", jobs: jobs, partdb: fakePartDB(t)}
}

func (h *harness) do(method, path string, form url.Values, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "http://"+h.host+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", h.srv.csrf)
	return h.do("POST", path, form)
}

func (h *harness) flash() string {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if h.srv.flash == nil {
		return ""
	}
	return h.srv.flash.Text
}

func TestGuard(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/v1/info", nil, func(r *http.Request) { r.Host = "evil.example:8631" }); w.Code != http.StatusForbidden {
		t.Errorf("foreign Host: %d", w.Code)
	}
	if w := h.do("GET", "/v1/info", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"test"`) {
		t.Errorf("info: %d %s", w.Code, w.Body)
	}
	if w := h.do("GET", "/", nil); w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("index: %d", w.Code)
	}
	if w := h.do("POST", "/routes", url.Values{"profile": {"2"}, "printer": {"ql"}}); w.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF: %d", w.Code)
	}
	form := url.Values{"csrf": {h.srv.csrf}, "profile": {"2"}, "printer": {"ql"}}
	// what a browser sends for a form on our own page
	browser := func(r *http.Request) { r.Header.Set("Origin", "http://"+h.host) }
	if w := h.do("POST", "/routes", form, browser); w.Code != http.StatusSeeOther {
		t.Errorf("same-origin browser POST: %d %s", w.Code, w.Body)
	}
	if w := h.do("GET", "/", nil); w.Header().Get("Referrer-Policy") == "no-referrer" {
		t.Error(`"no-referrer" makes browsers send "Origin: null" and breaks every form`)
	}
	if w := h.do("POST", "/routes", form, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", w.Code)
	}
}

func TestSetupRouteAndHandoff(t *testing.T) {
	h := newHarness(t)

	if w := h.post("/instance", url.Values{"base_url": {h.partdb.URL + "/"}, "token": {"tcp_wrong"}}); w.Code != http.StatusSeeOther || !strings.Contains(h.flash(), "401") {
		t.Fatalf("bad token accepted: %d %q", w.Code, h.flash())
	}
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})
	if f := h.flash(); f != "Connected to Part-DB" {
		t.Fatalf("setup: %q", f)
	}
	saved, err := os.ReadFile(h.srv.CfgPath)
	if err != nil || strings.Contains(string(saved), "tcp_test") {
		t.Fatalf("token must go to the keychain, not the file:\n%s", saved)
	}
	if tok, _ := keyring.Get(config.KeyringService, "default"); tok != "tcp_test" {
		t.Fatalf("keychain: %q", tok)
	}

	h.post("/routes", url.Values{"profile": {"2"}, "printer": {"ql"}})
	if f := h.flash(); f != "Profile bag 29 mm → ql" {
		t.Fatalf("route: %q", f)
	}
	page := h.do("GET", "/", nil).Body.String()
	m := regexp.MustCompile(`href="(javascript:[^"]+)"`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "Read-Only ✓") {
		t.Fatalf("no bookmarklet / token info on page:\n%s", page)
	}

	key := h.srv.config().Instances[0].PairingKey
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	q := url.Values{"src": {h.partdb.URL}, "type": {"part"}, "ids": {"32"}, "profile": {"2"}, "ts": {ts}}
	q.Set("sig", sign(key, canonical(h.partdb.URL, "part", "32", "2", ts)))

	if body := h.do("GET", "/v1/handoff?"+q.Encode(), nil).Body.String(); !strings.Contains(body, "Printed ✓") {
		t.Fatalf("signed handoff:\n%s", body)
	}
	select {
	case <-h.jobs:
	case <-time.After(10 * time.Second):
		t.Fatal("no job reached the printer")
	}
	// replay of the same link must not print again without a click
	if body := h.do("GET", "/v1/handoff?"+q.Encode(), nil).Body.String(); !strings.Contains(body, "already printed") {
		t.Errorf("replay printed:\n%s", body)
	}

	for name, mod := range map[string]func(url.Values){
		"unsigned": func(v url.Values) { v.Del("sig") },
		"tampered": func(v url.Values) { v.Set("ids", "33") },
		"expired": func(v url.Values) {
			old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
			v.Set("ts", old)
			v.Set("sig", sign(key, canonical(h.partdb.URL, "part", "32", "2", old)))
		},
	} {
		v := url.Values{}
		for k, vv := range q {
			v[k] = vv
		}
		mod(v)
		if body := h.do("GET", "/v1/handoff?"+v.Encode(), nil).Body.String(); !strings.Contains(body, "Print this label?") {
			t.Errorf("%s: expected a confirm page:\n%s", name, body)
		}
	}
	if body := h.do("GET", "/v1/handoff?"+strings.Replace(q.Encode(), "127.0.0.1", "evil.example", 1), nil).Body.String(); !strings.Contains(body, "not a configured Part-DB instance") {
		t.Errorf("foreign src:\n%s", body)
	}
	if body := h.do("GET", "/v1/handoff?src="+url.QueryEscape(h.partdb.URL)+"&type=part&ids=32&profile=9", nil).Body.String(); !strings.Contains(body, "No printer is set up") {
		t.Errorf("unrouted profile:\n%s", body)
	}

	// the confirm button posts back with the CSRF token and prints
	if body := h.post("/v1/handoff", url.Values{"src": {h.partdb.URL}, "type": {"part"}, "ids": {"32"}, "profile": {"2"}}).Body.String(); !strings.Contains(body, "Printed ✓") {
		t.Fatalf("confirmed handoff:\n%s", body)
	}
	<-h.jobs
	if n := len(h.srv.jobs); n != 2 {
		t.Errorf("%d jobs in history", n)
	}
}

func TestPrinterCRUDAndTestPrint(t *testing.T) {
	h := newHarness(t)
	h.post("/printers", url.Values{"id": {"ql2"}, "model": {"QL-820NWB"}, "host": {"127.0.0.1"}, "port": {"9100"}, "media": {"dk-11204"}, "rotate": {"auto"}, "cut": {"last"}})
	if p, err := h.srv.config().Printer("ql2"); err != nil || p.Media != "dk-11204" || p.Cut != "last" {
		t.Fatalf("add: %+v %v %q", p, err, h.flash())
	}
	h.post("/printers", url.Values{"id": {"bad"}, "model": {"QL-1"}, "host": {"x"}})
	if !strings.Contains(h.flash(), "unknown Brother QL model") {
		t.Errorf("bad model: %q", h.flash())
	}
	for i := range h.srv.cfg.Printers {
		if h.srv.cfg.Printers[i].ID == "ql2" {
			h.srv.cfg.Printers[i].SNMPCommunity, h.srv.cfg.Printers[i].AllowScaling, h.srv.cfg.Printers[i].NoSNMP = "s3cret", true, true
		}
	}
	h.post("/printers", url.Values{"id": {"ql2"}, "model": {"QL-820NWB"}, "host": {"127.0.0.1"}, "port": {"9100"}, "media": {"dk-11204"}, "rotate": {"auto"}, "cut": {"per_label"}})
	if p, _ := h.srv.config().Printer("ql2"); p.SNMPCommunity != "s3cret" || !p.AllowScaling || !p.NoSNMP || p.Cut != "per_label" {
		t.Errorf("editing in the UI dropped settings the form doesn't show: %+v", p)
	}
	h.post("/printers/ql2/delete", nil)
	if _, err := h.srv.config().Printer("ql2"); err == nil {
		t.Error("not deleted")
	}

	h.post("/printers/ql/test", nil)
	if !strings.HasPrefix(h.flash(), "Test label 1 page(s) on dk-22210") {
		t.Errorf("test print: %q", h.flash())
	}
	<-h.jobs

	w := h.do("GET", "/printers/ql/status", nil)
	if !strings.Contains(w.Body.String(), "no status over TCP") || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("status: %s", w.Body)
	}
	// without media, a printer that reports nothing can't print: flag it
	h.srv.cfg.Printers[0].Media = ""
	if w := h.do("GET", "/printers/ql/status", nil); !strings.Contains(w.Body.String(), "set media") || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("status without media: %s", w.Body)
	}
}

func TestStatusText(t *testing.T) {
	loaded := &driver.Media{ID: "dk-11201"}
	ready := &driver.Status{Code: driver.OK, Model: "QL-810W", Detail: "READY", Media: loaded}
	if text, ok := statusText(ready, &config.Printer{Media: "dk-11201"}); !ok || text != "OK · QL-810W · dk-11201 loaded · READY" {
		t.Errorf("match: %q %v", text, ok)
	}
	if text, ok := statusText(ready, &config.Printer{Media: "dk-22210"}); ok || !strings.Contains(text, "config expects dk-22210") {
		t.Errorf("mismatch: %q %v", text, ok)
	}
	if _, ok := statusText(ready, &config.Printer{}); !ok {
		t.Error("no configured media: any known roll is fine")
	}
	unknown := &driver.Status{Code: driver.OK, Model: "QL-810W", Media: &driver.Media{ID: "unknown"}}
	if text, ok := statusText(unknown, &config.Printer{}); ok || !strings.Contains(text, "set media") {
		t.Errorf("unknown roll, no configured media: %q %v", text, ok)
	}
	if _, ok := statusText(&driver.Status{Code: driver.OK}, &config.Printer{}); ok {
		t.Error("no roll reported, no configured media: jobs would fail")
	}
	if _, ok := statusText(unknown, &config.Printer{Media: "dk-22210"}); !ok {
		t.Error("unknown roll with configured media: printing falls back to the config")
	}
}

func TestRoutesKeepOtherInstances(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Instances = []config.Instance{{ID: "branch", BaseURL: h.partdb.URL}}
	h.srv.cfg.Routes = []config.Route{{Instance: "branch", Profile: 2, Printer: "ql"}}
	h.post("/routes", url.Values{"profile": {"2"}, "printer": {"ql"}})
	if rs := h.srv.config().Routes; len(rs) != 2 || rs[0].Instance != "branch" || rs[1].Instance != "" {
		t.Fatalf("saving a catch-all route replaced an instance route: %+v (%s)", rs, h.flash())
	}
	h.post("/routes/2/delete", url.Values{"instance": {""}})
	if rs := h.srv.config().Routes; len(rs) != 1 || rs[0].Instance != "branch" {
		t.Fatalf("deleting the catch-all route removed the instance route: %+v", rs)
	}
}

func TestGuardAllowsBoundLoopbackAddress(t *testing.T) {
	h := newHarness(t)
	h.srv.Addr = "127.0.0.2:8631"
	h.h = h.srv.Handler()
	if w := h.do("GET", "/v1/info", nil, func(r *http.Request) { r.Host = "127.0.0.2:8631" }); w.Code != 200 {
		t.Errorf("bound address rejected: %d", w.Code)
	}
	if w := h.do("GET", "/v1/info", nil, func(r *http.Request) { r.Host = "127.0.0.3:8631" }); w.Code != http.StatusForbidden {
		t.Errorf("other loopback address accepted: %d", w.Code)
	}
}

func TestPrintFileRecordsJob(t *testing.T) {
	h := newHarness(t)
	if err := h.srv.PrintFile(context.Background(), "hot folder", "ql", "../../testdata/sample-29x46.pdf"); err != nil {
		t.Fatal(err)
	}
	<-h.jobs
	if err := h.srv.PrintFile(context.Background(), "hot folder", "ql", filepath.Join(t.TempDir(), "missing.pdf")); err == nil {
		t.Error("missing file printed")
	}
	if len(h.srv.jobs) != 2 || h.srv.jobs[1].Source != "hot folder" || h.srv.jobs[1].What != "sample-29x46.pdf" || h.srv.jobs[0].Err == "" {
		t.Errorf("history: %+v", h.srv.jobs)
	}
}
