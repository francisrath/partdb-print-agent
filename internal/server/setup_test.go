package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
)

// fakeQLCandidate is what discovery reports for the harness's fake network QL.
func fakeQLCandidate(t *testing.T, h *harness) discovery.Candidate {
	p := h.srv.cfg.Printers[0]
	m, _ := devicedb.Default().Model("QL-810W")
	return discovery.Candidate{Kind: "tcp", Host: p.Transport.Host, Port: p.Transport.Port, Model: m, ModelID: m.ID, Status: discovery.Supported}
}

// partDBWith serves the harness's fake Part-DB, with handlers replacing some paths.
func partDBWith(t *testing.T, h *harness, override map[string]http.HandlerFunc) string {
	target, _ := url.Parse(h.partdb.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, f := range override {
			if strings.HasPrefix(r.URL.Path, prefix) {
				f(w, r)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func wantRedirect(t *testing.T, w *httptest.ResponseRecorder, to string) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != to {
		t.Fatalf("want redirect to %s, got %d %q: %s", to, w.Code, w.Header().Get("Location"), w.Body)
	}
}

// wizardToTest connects Part-DB at partDB, picks the fake QL and prints the first test label
// (choosing the tape, as the fake reports none).
func wizardToTest(t *testing.T, h *harness, partDB string) {
	t.Helper()
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{fakeQLCandidate(t, h)}, nil
	}
	wantRedirect(t, h.do("GET", "/setup/", nil), "/setup/partdb")
	wantRedirect(t, h.post("/setup/partdb", url.Values{"base_url": {partDB}, "token": {"tcp_test"}}), "/setup/printer")
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "QL-810W") {
		t.Fatalf("printer page: %d %s", w.Code, w.Body)
	}
	wantRedirect(t, h.post("/setup/printer", url.Values{"choice": {"0"}}), "/setup/test")
	h.srv.local.Draft.Printer.NoSNMP = true // the fake QL has no SNMP; skip the 2 s timeouts
	if w := h.post("/setup/test", url.Values{"action": {"print"}}); !strings.Contains(w.Body.String(), "Which tape") {
		t.Fatalf("no tape question for a printer that reports none: %s", w.Body)
	}
	if w := h.post("/setup/test", url.Values{"action": {"media"}, "media": {"dk-22210"}}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Looks right") {
		t.Fatalf("after picking the tape: %d %s", w.Code, w.Body)
	}
	<-h.jobs
}

func agentAPI(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
}

func TestWizardHappyPath(t *testing.T) {
	h := newHarness(t)
	var started []config.Instance
	h.srv.OnInstance = func(in config.Instance) { started = append(started, in) }
	partDB := partDBWith(t, h, map[string]http.HandlerFunc{"/en/agent-api/": agentAPI(http.StatusMethodNotAllowed)})
	wizardToTest(t, h, partDB)
	wantRedirect(t, h.post("/setup/test", url.Values{"action": {"right"}}), "/setup/name")
	if w := h.do("GET", "/setup/name", nil); !strings.Contains(w.Body.String(), "Receiving desk") {
		t.Errorf("name page lacks suggestions: %s", w.Body)
	}
	wantRedirect(t, h.post("/setup/name", url.Values{"name": {"Receiving desk"}}), "/setup/done")
	if w := h.do("GET", "/setup/done", nil); !strings.Contains(w.Body.String(), "Part-DB now shows") {
		t.Errorf("done page: %s", w.Body)
	}

	cfg, err := config.Load(h.srv.CfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Printer("receiving-desk")
	want := fakeQLCandidate(t, h)
	if err != nil || p.Name != "Receiving desk" || p.Model != "QL-810W" || p.Media != "dk-22210" ||
		p.Transport.Kind != "tcp" || p.Transport.Host != want.Host || p.Transport.Port != want.Port || p.Mirror || p.UpsideDown {
		t.Errorf("saved printer %+v %v", p, err)
	}
	if len(cfg.Instances) != 1 || cfg.Instances[0].BaseURL != partDB {
		t.Errorf("saved instances %+v", cfg.Instances)
	}
	if len(started) != 1 || started[0].Token != "tcp_test" {
		t.Errorf("jobs not started for the new Part-DB: %+v", started)
	}
}

func TestFinishWritesFixes(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	for _, a := range []string{"mirrored", "upside-down"} {
		if w := h.post("/setup/test", url.Values{"action": {a}}); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", a, w.Code, w.Body)
		}
		<-h.jobs // each fix prints the card again
	}
	h.do("GET", "/setup/printer", nil) // Back …
	h.do("GET", "/setup/test", nil)    // … and Next keep the fixes
	h.post("/setup/test", url.Values{"action": {"right"}})
	wantRedirect(t, h.post("/setup/name", url.Values{"name": {"Desk"}}), "/setup/done")
	p, err := h.srv.config().Printer("desk")
	if err != nil || !p.Mirror || !p.UpsideDown {
		t.Errorf("fixes not saved: %+v %v", p, err)
	}
}

func TestCutOffPicksMedia(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	if w := h.post("/setup/test", url.Values{"action": {"cut-off"}}); !strings.Contains(w.Body.String(), "Which tape") {
		t.Errorf("cut off on a printer that reports no tape should ask for it: %s", w.Body)
	}
}

// A printer that reports its roll got a card sized for it: cut off is a loading problem,
// not a setting, so the wizard says what to check and offers the report.
func TestCutOffWithReportedMedia(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	h.srv.local.Draft.MediaReported = true
	w := h.post("/setup/test", url.Values{"action": {"cut-off"}})
	if body := w.Body.String(); !strings.Contains(body, "against the guide") || !strings.Contains(body, `action="/setup/report"`) {
		t.Errorf("cut off with a reported roll: %s", body)
	}
	if got := h.srv.local.Draft.Printer.Rotate; got != "" {
		t.Errorf("rotate changed to %q", got)
	}
}

func TestNoAgentAPIMessage(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, map[string]http.HandlerFunc{"/en/agent-api/": agentAPI(http.StatusNotFound)}))
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "can't send print jobs") {
		t.Errorf("printer page doesn't say this Part-DB can't send jobs: %s", w.Body)
	}
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"Desk"}})
	if w := h.do("GET", "/setup/done", nil); strings.Contains(w.Body.String(), "Part-DB now shows") {
		t.Errorf("done page promises Part-DB printing: %s", w.Body)
	}
}

func TestReadOnlyWarning(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, map[string]http.HandlerFunc{
		"/api/tokens/current": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"name":"agent","user":{"name":"fran"},"level":3}`)
		}}))
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "read-only token is enough") {
		t.Errorf("no warning for a token that can change data: %s", w.Body)
	}
}

func TestSlugClash(t *testing.T) {
	h := newHarness(t) // already has printer "ql"
	wizardToTest(t, h, partDBWith(t, h, nil))
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"QL"}})
	if p, err := h.srv.config().Printer("ql-2"); err != nil || p.Name != "QL" {
		t.Errorf("second printer named QL: %+v %v", p, err)
	}
	if p, _ := h.srv.config().Printer("ql"); p.Name == "QL" && p.Transport.Kind != "tcp" {
		t.Error("existing printer overwritten")
	}
}

func TestFixRoundsOfferReport(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	var body string
	for range 3 {
		w := h.post("/setup/test", url.Values{"action": {"mirrored"}})
		<-h.jobs
		body = w.Body.String()
	}
	if !strings.Contains(body, `action="/setup/report"`) {
		t.Errorf("no report offered after 3 rounds: %s", body)
	}
}

func TestReportDownload(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	if w := h.do("GET", "/setup/report", nil); !strings.Contains(w.Body.String(), "github.com/francisrath/partdb-print-agent/issues/new") {
		t.Errorf("report page: %s", w.Body)
	}
	w := h.post("/setup/report", url.Values{})
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(w.Body.String(), `"tool"`) {
		t.Errorf("report download: %v %s", w.Header(), w.Body)
	}
	host := h.srv.cfg.Printers[0].Transport.Host
	if strings.Contains(w.Body.String(), host+":"+strconv.Itoa(h.srv.cfg.Printers[0].Transport.Port)) || strings.Contains(w.Body.String(), `"`+host+`"`) {
		t.Error("report not redacted")
	}
}

func TestSettingsLinksToWizard(t *testing.T) {
	h := newHarness(t)
	if !strings.Contains(h.do("GET", "/", nil).Body.String(), `href="/setup/"`) {
		t.Error("settings page has no Add a printer link")
	}
}
