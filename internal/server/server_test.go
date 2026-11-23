package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"html/template"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
	"github.com/francisrath/pdb-print/internal/partdb"
	"github.com/francisrath/pdb-print/internal/printing"
	"github.com/francisrath/pdb-print/internal/selfupdate"
	"github.com/francisrath/pdb-print/internal/telemetry"
	"github.com/francisrath/pdb-print/internal/update"
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
	cfg := &config.Config{Printers: []config.Printer{{ID: "ql", Model: "QL-810W", Media: "dk-22210", NoSNMP: true,
		Transport: config.Transport{Kind: "tcp", Host: qlHost, Port: port}}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &printing.Service{Log: log}
	t.Cleanup(svc.Close)
	s := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), svc, log, "127.0.0.1:8631", "test")
	s.LANAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.168.1.23")} } // lanReq's address
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
		body := h.do("GET", "/v1/handoff?"+v.Encode(), nil).Body.String()
		if !strings.Contains(body, "Print this label?") {
			t.Errorf("%s: expected a confirm page:\n%s", name, body)
		}
		if name != "expired" && !strings.Contains(body, "Part-DB") {
			t.Errorf("%s: the reason should cover Part-DB's print button too:\n%s", name, body)
		}
	}
	if body := h.do("GET", "/v1/handoff?"+strings.Replace(q.Encode(), "127.0.0.1", "evil.example", 1), nil).Body.String(); !strings.Contains(body, "not a configured Part-DB instance") {
		t.Errorf("foreign src:\n%s", body)
	}
	if body := h.do("GET", "/v1/handoff?src="+url.QueryEscape(h.partdb.URL)+"&type=part&ids=32&profile=9", nil).Body.String(); !strings.Contains(body, "No printer is set up") {
		t.Errorf("unrouted profile:\n%s", body)
	}

	// the confirm button posts back with the CSRF token and prints
	if body := h.do("POST", "/v1/handoff", url.Values{"src": {h.partdb.URL}, "type": {"part"}, "ids": {"32"}, "profile": {"2"}, "csrf": {h.srv.handoffCSRF}}).Body.String(); !strings.Contains(body, "Printed ✓") {
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
	if !strings.Contains(w.Body.String(), "no status over tcp") || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("status: %s", w.Body)
	}
	// without media, a printer that reports nothing can't print: flag it
	h.srv.cfg.Printers[0].Media = ""
	if w := h.do("GET", "/printers/ql/status", nil); !strings.Contains(w.Body.String(), "set media") || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("status without media: %s", w.Body)
	}
}

func TestStatusText(t *testing.T) {
	loaded := &devicedb.Media{ID: "dk-11201"}
	ql := func(media string, allowed ...string) *config.Printer {
		return &config.Printer{Model: "QL-810W", Media: media, MediaAllowed: allowed}
	}
	ready := &driver.Status{Code: driver.OK, Model: "QL-810W", Detail: "READY", Media: loaded}
	if text, ok := statusText(&printing.Service{}, ready, ql("dk-22210")); !ok || text != "OK · QL-810W · media: dk-11201 (detected) · READY" {
		t.Errorf("detected beats config: %q %v", text, ok)
	}
	if text, ok := statusText(&printing.Service{}, ready, ql("", "dk-22210")); ok || !strings.Contains(text, "not in media_allowed") {
		t.Errorf("not allowed: %q %v", text, ok)
	}
	unknown := &driver.Status{Code: driver.OK, Model: "QL-810W", Media: &devicedb.Media{ID: "unknown"}}
	if text, ok := statusText(&printing.Service{}, unknown, ql("auto")); ok || !strings.Contains(text, "set media") {
		t.Errorf("unknown roll, no configured media: %q %v", text, ok)
	}
	if _, ok := statusText(&printing.Service{}, &driver.Status{Code: driver.OK}, ql("")); ok {
		t.Error("no roll reported, no configured media: jobs would fail")
	}
	// a printer that doesn't name itself in its status (a Zebra) shows the configured model, no empty field
	silentModel := &driver.Status{Code: driver.OK, Media: loaded}
	if text, _ := statusText(&printing.Service{}, silentModel, ql("")); text != "OK · QL-810W · media: dk-11201 (detected)" {
		t.Errorf("no model in status: %q", text)
	}
	if text, ok := statusText(&printing.Service{}, unknown, ql("dk-22210")); !ok || !strings.Contains(text, "dk-22210 (config fallback)") {
		t.Errorf("unknown roll with configured media: %q %v", text, ok)
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

func TestConnectLinkAndLongIDs(t *testing.T) {
	h := newHarness(t)
	if page := h.do("GET", "/", nil).Body.String(); strings.Contains(page, "/label/agent#") {
		t.Fatal("connect link without a Part-DB instance")
	}
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})
	h.post("/routes", url.Values{"profile": {"2"}, "printer": {"ql"}})
	key := h.srv.config().Instances[0].PairingKey

	page := h.do("GET", "/", nil).Body.String()
	want := `href="` + h.partdb.URL + `/label/agent#url=http%3a%2f%2f127.0.0.1%3a8631&amp;key=` + key + `"`
	if !strings.Contains(strings.ToLower(page), strings.ToLower(want)) {
		t.Fatalf("no connect link %s on page:\n%s", want, page)
	}

	// bulk prints from Part-DB's table action list every id: 300 parts must fit
	var ids []string
	for i := 1; i <= 300; i++ {
		ids = append(ids, strconv.Itoa(i))
	}
	handoff := func(ids string) string {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		q := url.Values{"src": {h.partdb.URL}, "type": {"part"}, "ids": {ids}, "profile": {"2"}, "ts": {ts}}
		q.Set("sig", sign(key, canonical(h.partdb.URL, "part", ids, "2", ts)))
		return h.do("GET", "/v1/handoff?"+q.Encode(), nil).Body.String()
	}
	if body := handoff(strings.Join(ids, ",")); !strings.Contains(body, "Printed ✓") {
		t.Fatalf("300 ids:\n%s", body)
	}
	<-h.jobs
	if src := h.srv.jobs[0].Source; src != "browser" { // bookmarklet or Part-DB's print button
		t.Errorf("job source %q", src)
	}
	if body := handoff(strings.Repeat("1,", 5000) + "1"); !strings.Contains(body, "Too many labels for one request") {
		t.Errorf("10 kB of ids:\n%s", body)
	}
	if body := handoff("3,,5"); !strings.Contains(body, "Invalid element ids.") {
		t.Errorf("malformed ids:\n%s", body)
	}
}

func TestAgentJobsHooks(t *testing.T) {
	h := newHarness(t)
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})

	id, err := h.srv.EnsureAgentID()
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("agent id %q %v", id, err)
	}
	if again, _ := h.srv.EnsureAgentID(); again != id {
		t.Errorf("agent id changed: %q → %q", id, again)
	}
	if saved, _ := os.ReadFile(h.srv.CfgPath); !strings.Contains(string(saved), "agent_id: "+id) {
		t.Errorf("agent id not saved:\n%s", saved)
	}

	if ps := h.srv.AgentPrinters(); len(ps) != 1 || ps[0].ID != "ql" || ps[0].Model != "QL-810W" || ps[0].Media != "dk-22210" {
		t.Errorf("printers: %+v", ps)
	}

	var job partdb.Job
	job.ID, job.Type, job.Printer = 7, "print", "ql"
	job.Payload.ProfileID, job.Payload.ElementType, job.Payload.ElementIDs = 2, "part", "32"
	res, err := h.srv.PrintJob(context.Background(), h.srv.config().Instances[0].ID, job)
	if err != nil || !strings.Contains(res, "page(s)") {
		t.Fatalf("print job: %q %v", res, err)
	}
	<-h.jobs
	if j := h.srv.jobs[0]; j.Source != "Part-DB job" || !strings.Contains(j.What, "part 32") {
		t.Errorf("history: %+v", j)
	}

	job.Printer = "gone"
	if _, err := h.srv.PrintJob(context.Background(), h.srv.config().Instances[0].ID, job); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("unknown printer: %v", err)
	}
}

func TestSavePrinterKeepsNonQL(t *testing.T) {
	h := newHarness(t)
	pt := config.Printer{ID: "pt", Model: "PT-9500PC", Media: "tze-24", Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}}
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, pt)
	// the form as the UI sends it for this printer: its own model and media, which the QL checks don't know
	h.post("/printers", url.Values{"id": {"pt"}, "model": {"PT-9500PC"}, "host": {"127.0.0.1"}, "port": {"9100"}, "media": {"tze-24"}})
	if !strings.Contains(h.flash(), "config file") {
		t.Errorf("flash %q", h.flash())
	}
	if p, err := h.srv.config().Printer("pt"); err != nil || p.Model != "PT-9500PC" || p.Transport.Device != "/dev/usb/lp0" {
		t.Errorf("non-QL printer overwritten from the UI: %+v %v", p, err)
	}
	if w := h.do("GET", "/", nil); !strings.Contains(w.Body.String(), "/dev/usb/lp0") {
		t.Error("printer table doesn't show the USB device")
	}
}

// silentPT is a USB printer that never answers.
type silentPT struct{}

func (silentPT) Open(context.Context) error  { return nil }
func (silentPT) Write(p []byte) (int, error) { return len(p), nil }
func (silentPT) Read([]byte) (int, error)    { return 0, os.ErrDeadlineExceeded }
func (silentPT) Close() error                { return nil }
func (silentPT) Kind() string                { return "usb" }

// A PT that gives no status can't print (jobs need its replies), unlike a QL over TCP.
func TestPTStatusWithoutReplyIsNotOK(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "pt", Model: "PT-9500PC", Media: "tze-24",
		Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}})
	h.srv.Svc.OpenTransport = func(*config.Printer) driver.Transport { return silentPT{} }
	w := h.do("GET", "/printers/pt/status", nil)
	if body := w.Body.String(); !strings.Contains(body, `"ok":false`) || strings.Contains(body, "TCP") {
		t.Errorf("silent PT: %s", body)
	}
}

// The hint for a silent printer fits how it is connected.
func TestStatusWithoutReplyHint(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers,
		config.Printer{ID: "pt", Model: "PT-9500PC", Media: "tze-24", Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}},
		config.Printer{ID: "cat", Model: "MX10", Media: "paper-57", Transport: config.Transport{Kind: "ble", Name: "MX10"}})
	h.srv.Svc.OpenTransport = func(*config.Printer) driver.Transport { return silentPT{} }
	for id, want := range map[string]string{"pt": "USB cable", "cat": "switched on, in range and not connected to a phone"} {
		if body := h.do("GET", "/printers/"+id+"/status", nil).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s: %s, want %q", id, body, want)
		}
	}
}

func TestCheckHintBT(t *testing.T) {
	if got := checkHint("bt"); !strings.Contains(got, "paired with this computer") {
		t.Errorf("bt hint %q", got)
	}
}

func TestResultTextCarriesWarnings(t *testing.T) {
	got := resultText(&printing.Result{Pages: 1, Media: "tze-36", Confirmed: true,
		Warnings: []string{"label has ink on the tape's unprintable edges (4.5 mm); it was trimmed"}})
	if want := "1 page(s) on tze-36; warning: label has ink on the tape's unprintable edges (4.5 mm); it was trimmed"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// Whether a silent printer is a problem depends on the model, not on the driver name used.
func TestPTStatusWithoutReplyFamilyName(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "pt", Model: "PT-9500PC", Media: "tze-24",
		Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}})
	h.srv.Svc.OpenTransport = func(*config.Printer) driver.Transport { return silentPT{} }
	if body := h.do("GET", "/printers/pt/status", nil).Body.String(); !strings.Contains(body, `"ok":false`) {
		t.Errorf("silent PT: %s", body)
	}
}

func TestFormMediaTaggedWithModels(t *testing.T) {
	h := newHarness(t)
	if body := h.do("GET", "/", nil).Body.String(); !strings.Contains(body, ` QL-810W QL-820NWB"`) {
		t.Error("media options don't say which models take them")
	}
}

func TestAgentPrintersReportOnline(t *testing.T) {
	h := newHarness(t)
	h.srv.Svc.OnlineCheck = func(p *config.Printer) bool { return p.ID == "ql" }
	ps := h.srv.AgentPrinters()
	if len(ps) != 1 || !ps[0].Online {
		t.Errorf("%+v", ps)
	}
	b, _ := json.Marshal(ps[0])
	if !strings.Contains(string(b), `"online":true`) {
		t.Errorf("heartbeat JSON %s", b)
	}
}

// Editing a printer in Settings keeps the fixes the wizard's test print set.
func TestSavePrinterKeepsFixes(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers[0].Mirror, h.srv.cfg.Printers[0].UpsideDown = true, true
	p := h.srv.cfg.Printers[0]
	h.post("/printers", url.Values{"id": {p.ID}, "name": {"Desk"}, "model": {p.Model}, "host": {p.Transport.Host},
		"port": {"9100"}, "media": {p.Media}})
	got, err := h.srv.config().Printer(p.ID)
	if err != nil || !got.Mirror || !got.UpsideDown || got.Name != "Desk" {
		t.Errorf("after edit: %+v %v", got, err)
	}
}

// Connecting a Part-DB starts fetching its jobs without a restart.
func TestSaveInstanceStartsJobs(t *testing.T) {
	h := newHarness(t)
	var got []config.Instance
	h.srv.OnInstance = func(in config.Instance) { got = append(got, in) }
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {""}})
	if len(got) != 0 {
		t.Fatalf("called without a token: %+v", got)
	}
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})
	if len(got) != 1 || got[0].BaseURL != h.partdb.URL || got[0].Token != "tcp_test" {
		t.Errorf("OnInstance calls: %+v", got)
	}
}

// The printer table shows each transport's own address, and a route without a Part-DB profile
// name shows just the profile number.
func TestIndexAddressesAndRoutes(t *testing.T) {
	h := newHarness(t)
	h.srv.mu.Lock()
	h.srv.cfg.Printers = append(h.srv.cfg.Printers,
		config.Printer{ID: "cat", Model: "MX10", Transport: config.Transport{Kind: "ble", Name: "MX10", Address: "AA:BB:CC:DD:EE:01"}},
		config.Printer{ID: "rego", Model: "RG-MLP80B", Transport: config.Transport{Kind: "bt", Name: "RG-MLP80B"}},
		config.Printer{ID: "pt", Model: "PT-9500PC", Transport: config.Transport{Kind: "usb", USBID: "04f9:2002"}},
		config.Printer{ID: "lp", Model: "LP2844", Transport: config.Transport{Kind: "serial", Device: "/dev/ttyUSB0"}})
	h.srv.cfg.Routes = append(h.srv.cfg.Routes, config.Route{Profile: 2, Printer: "ql"})
	h.srv.mu.Unlock()
	body := h.do("GET", "/", nil).Body.String()
	for _, want := range []string{"Bluetooth AA:BB:CC:DD:EE:01", "Bluetooth RG-MLP80B", "04f9:2002", "<td>/dev/ttyUSB0</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("address %q missing", want)
		}
	}
	if strings.Contains(body, ">:0<") {
		t.Error("a Bluetooth or serial printer shows as :0")
	}
	if !strings.Contains(body, "<td>2</td>") {
		t.Error("route without a profile name should show just its number")
	}
}

func TestIndexShowsNewerRelease(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.0.0","html_url":"https://example.test/notes"}`))
	}))
	defer gh.Close()
	h := newHarness(t)
	if body := h.do("GET", "/", nil).Body.String(); strings.Contains(body, "is out") {
		t.Fatal("banner without a check")
	}
	h.srv.Updates = &update.Checker{Current: "0.2.0", URL: gh.URL}
	if _, err := h.srv.Updates.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := h.do("GET", "/", nil).Body.String()
	for _, want := range []string{"pdb-print 9.0.0 is out", `href="https://example.test/notes"`} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing", want)
		}
	}
}

func TestTelemetryChoice(t *testing.T) {
	h := newHarness(t)
	body := h.do("GET", "/", nil).Body.String()
	if !strings.Contains(body, "Nothing is sent until you choose") || !strings.Contains(body, `name="on" value="1" checked`) {
		t.Fatalf("undecided page: %s", body)
	}
	h.post("/telemetry", url.Values{}) // unticked
	if on := h.srv.config().Telemetry; on == nil || *on {
		t.Fatalf("telemetry %v, want false", on)
	}
	body = h.do("GET", "/", nil).Body.String()
	if strings.Contains(body, "Nothing is sent until you choose") || strings.Contains(body, `name="on" value="1" checked`) {
		t.Error("decided off: notice or ticked box still shown")
	}
	h.post("/telemetry", url.Values{"on": {"1"}})
	if on := h.srv.config().Telemetry; on == nil || !*on {
		t.Errorf("telemetry %v, want true", on)
	}
	t.Setenv("DO_NOT_TRACK", "1")
	if body := h.do("GET", "/", nil).Body.String(); !strings.Contains(body, "Not sent: DO_NOT_TRACK is set") {
		t.Error("environment opt-out not shown")
	}
}

func TestJobsAreCounted(t *testing.T) {
	h := newHarness(t)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("PDB_PRINT_TELEMETRY", "")
	h.srv.Stats = telemetry.Open(filepath.Join(t.TempDir(), "telemetry.json"))
	h.post("/printers/ql/test", nil) // undecided: not counted
	if r := (&telemetry.Sender{Store: h.srv.Stats, Version: "0.3.0", Config: h.srv.Config}).Build(); len(r.Jobs) != 0 {
		t.Fatalf("counted while undecided: %+v", r.Jobs)
	}
	h.post("/telemetry", url.Values{"on": {"1"}})
	h.post("/printers/ql/test", nil)
	h.srv.finish("hot folder", "x.pdf", "ql", nil, &driver.Error{Code: driver.NoMedia})
	h.srv.finish("something new", "x.pdf", "nope", nil, errors.New("boom"))
	r := (&telemetry.Sender{Store: h.srv.Stats, Version: "0.3.0", Config: h.srv.Config}).Build()
	if r.Jobs["web-ui"] != 1 || r.Jobs["hot-folder"] != 1 || r.Jobs["other"] != 1 || r.Labels["QL-810W"] != 1 ||
		r.Errors["NO_MEDIA"] != 1 || r.Errors["OTHER"] != 1 {
		t.Errorf("%+v", r)
	}
}

// A fresh install (no Part-DB, no printer) opens the step-by-step wizard instead of the settings
// page; "?manual" still shows the page, and the wizard links to it.
func TestIndexFreshInstallOpensWizard(t *testing.T) {
	h := newHarness(t)
	h.srv.mu.Lock()
	h.srv.cfg.Printers = nil
	h.srv.mu.Unlock()
	if rec := h.do("GET", "/", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup/" {
		t.Fatalf("GET / on a fresh install: %d %q, want 303 to /setup/", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.do("GET", "/?manual", nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /?manual: %d, want the settings page", rec.Code)
	}
	if body := h.do("GET", "/setup/partdb", nil).Body.String(); !strings.Contains(body, `href="/?manual"`) {
		t.Error("the wizard's first step has no way to the settings page")
	}
}

// On a fresh install "/" bounces to the wizard, so a settings form posted from "/?manual" must
// come back to "/?manual" with its message, not to the wizard.
func TestFreshInstallFormKeepsSettingsPage(t *testing.T) {
	h := newHarness(t)
	h.srv.mu.Lock()
	h.srv.cfg.Printers = nil
	h.srv.mu.Unlock()
	rec := h.post("/telemetry", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?manual" {
		t.Fatalf("POST /telemetry: %d %q, want 303 to /?manual", rec.Code, rec.Header().Get("Location"))
	}
	if body := h.do("GET", "/?manual", nil).Body.String(); !strings.Contains(body, "flash") {
		t.Error("the form's message is not on the settings page")
	}
}

// withRelease gives h a release check that found 9.0.0 and an Updater whose downloads go to dl.
func withRelease(t *testing.T, h *harness, dl http.HandlerFunc) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.0.0","html_url":"https://example.test/notes"}`))
	}))
	t.Cleanup(gh.Close)
	h.srv.Updates = &update.Checker{Current: "0.2.0", URL: gh.URL}
	if _, err := h.srv.Updates.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(dl)
	t.Cleanup(srv.Close)
	h.srv.Updater = &selfupdate.Updater{Base: srv.URL, Dir: t.TempDir(), Quiesce: h.srv.Svc.Quiesce,
		Restart: func() error { t.Error("restarted"); return nil }}
}

func TestUpdateButtonShown(t *testing.T) {
	h := newHarness(t)
	withRelease(t, h, http.NotFound)
	body := h.do("GET", "/", nil).Body.String()
	if !strings.Contains(body, `action="/update"`) || !strings.Contains(body, "Update now") || strings.Contains(body, "To update:") {
		t.Errorf("no Update now button: %s", body)
	}
}

func TestUpdateButtonHiddenWhenOff(t *testing.T) {
	off := false
	for name, set := range map[string]func(h *harness){
		"self_update: false": func(h *harness) { h.srv.cfg.SelfUpdate = &off },
		"no updater":         func(h *harness) { h.srv.Updater = nil },
	} {
		h := newHarness(t)
		withRelease(t, h, http.NotFound)
		set(h)
		body := h.do("GET", "/", nil).Body.String()
		if strings.Contains(body, `action="/update"`) || !strings.Contains(body, "To update:") {
			t.Errorf("%s: button shown or hint missing: %s", name, body)
		}
		if w := h.post("/update", nil); w.Code != http.StatusNotFound {
			t.Errorf("%s: POST /update: %d", name, w.Code)
		}
	}
}

// Review Focus 3: "Update now" in two tabs. The second request is refused and nothing is fetched twice.
func TestUpdateTwice409(t *testing.T) {
	h := newHarness(t)
	unblock := make(chan struct{})
	var mu sync.Mutex
	gets := 0
	withRelease(t, h, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gets++
		mu.Unlock()
		<-unblock
		http.NotFound(w, r)
	})
	if w := h.post("/update", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("first POST: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(h.do("GET", "/?manual", nil).Body.String(), "Updating to 9.0.0") {
		t.Error("no running notice")
	}
	if w := h.post("/update", nil); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "reload the page in a moment") {
		t.Errorf("second POST: %d %q, want 409 with a message for people", w.Code, w.Body)
	}
	if w := h.do("POST", "/update", url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF token: %d", w.Code)
	}
	close(unblock)
	for end := time.Now().Add(5 * time.Second); h.srv.updating.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("update still running")
		}
	}
	if body := h.do("GET", "/?manual", nil).Body.String(); !strings.Contains(body, "Update failed: ") {
		t.Errorf("no failure shown: %s", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 1 {
		t.Errorf("%d downloads, want 1", gets)
	}
}

// A download that fails at once: its error is on the page (every tab, until the next Update now),
// never replaced by the "Updating" notice, and no "reloads" text shows without the element that polls.
func TestUpdateFailsFast(t *testing.T) {
	h := newHarness(t)
	withRelease(t, h, http.NotFound)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // refuses connections
	h.srv.Updater.Base = dead.URL
	if w := h.post("/update", nil); w.Code != http.StatusSeeOther {
		t.Fatalf("POST: %d", w.Code)
	}
	h.srv.updateWG.Wait()
	body := h.do("GET", "/?manual", nil).Body.String()
	if strings.Count(body, "Update failed: ") != 1 {
		t.Errorf("failure not shown once: %s", body)
	}
	if strings.Contains(body, "reloads") && !strings.Contains(body, "data-updating") {
		t.Error(`"reloads" without polling`)
	}
	if body := h.do("GET", "/?manual", nil).Body.String(); strings.Count(body, "Update failed: ") != 1 || !strings.Contains(body, `action="/update"`) {
		t.Errorf("second page (another tab): failure gone, or no button to retry: %s", body)
	}
	// the next Update now clears it while it runs
	unblock := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-unblock; http.NotFound(w, r) }))
	defer slow.Close()
	h.srv.Updater.Base = slow.URL
	h.post("/update", nil)
	if body := h.do("GET", "/?manual", nil).Body.String(); strings.Contains(body, "Update failed") {
		t.Error("old failure shown while the next update runs")
	}
	close(unblock)
	h.srv.updateWG.Wait()
}

// Swapped but not restarted: the page says to restart instead of offering the same update again.
func TestUpdateInstalledNotRestarted(t *testing.T) {
	h := newHarness(t)
	withRelease(t, h, http.NotFound)
	h.srv.updateResult(selfupdate.ErrInstalled, "9.0.0")
	body := h.do("GET", "/?manual", nil).Body.String()
	if strings.Contains(body, `action="/update"`) || !strings.Contains(body, "pdb-print 9.0.0 is installed; restart pdb-print") {
		t.Errorf("after a failed restart: %s", body)
	}
}

// A Server not made by New: StopUpdate doesn't panic.
func TestStopUpdateZeroServer(t *testing.T) {
	(&Server{}).StopUpdate()
}

// The hint follows the current self_update setting when the page is rendered.
func TestUpdateHintAtRender(t *testing.T) {
	h := newHarness(t)
	withRelease(t, h, http.NotFound)
	off := false
	h.srv.cfg.SelfUpdate = &off
	body := h.do("GET", "/?manual", nil).Body.String()
	if want := update.Hint("9.0.0", false); !strings.Contains(body, template.HTMLEscapeString(want)) {
		t.Errorf("hint %q missing: %s", want, body)
	}
}

// A stale tab after a swap whose restart failed: no second download, back to the restart notice.
func TestUpdateAgainAfterInstalled(t *testing.T) {
	h := newHarness(t)
	gets := 0
	withRelease(t, h, func(w http.ResponseWriter, r *http.Request) { gets++; http.NotFound(w, r) })
	h.srv.updateResult(selfupdate.ErrInstalled, "9.0.0")
	if w := h.post("/update", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?manual" {
		t.Fatalf("POST: %d %s", w.Code, w.Header().Get("Location"))
	}
	h.srv.updateWG.Wait()
	if gets != 0 || h.srv.updating.Load() {
		t.Errorf("%d downloads, updating %v", gets, h.srv.updating.Load())
	}
}

// localhost and [::1] are ours only while we listen on [::1] too: with IPv4Only (the service
// couldn't bind [::1]) a program there could share our origin, so those Host headers are refused.
func TestGuardLoopbackHosts(t *testing.T) {
	for _, v4only := range []bool{false, true} {
		t.Run(fmt.Sprint("IPv4Only ", v4only), func(t *testing.T) {
			h := newHarness(t)
			h.srv.IPv4Only = v4only
			h.h = h.srv.Handler()
			for host, ok := range map[string]bool{"127.0.0.1:8631": true, "localhost:8631": !v4only, "[::1]:8631": !v4only} {
				w := h.do("GET", "/v1/info", nil, func(r *http.Request) { r.Host = host })
				if (w.Code == 200) != ok {
					t.Errorf("Host %s: %d, want allowed %v", host, w.Code, ok)
				}
			}
		})
	}
}
