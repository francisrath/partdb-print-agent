package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
	"net"
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
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/zalando/go-keyring"
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
	wizardToTestWith(t, h, url.Values{"base_url": {partDB}, "token": {"tcp_test"}})
}

// wizardToTestWith is wizardToTest with the Part-DB form as given; start is the first page.
func wizardToTestWith(t *testing.T, h *harness, form url.Values, start ...string) {
	t.Helper()
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{fakeQLCandidate(t, h)}, nil
	}
	if len(start) == 0 {
		wantRedirect(t, h.do("GET", "/setup/", nil), "/setup/partdb")
	} else if w := h.do("GET", start[0], nil); w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", start[0], w.Code)
	}
	wantRedirect(t, h.post("/setup/partdb", form), "/setup/printer")
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

// Back to the printer page and Next with the same printer keeps the test print and its fixes.
func TestBackNextKeepsFixes(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	h.post("/setup/test", url.Values{"action": {"mirrored"}})
	<-h.jobs
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), `value="0" checked`) {
		t.Errorf("current printer not pre-selected: %s", w.Body)
	}
	wantRedirect(t, h.post("/setup/printer", url.Values{"choice": {"0"}}), "/setup/test")
	if d := h.srv.local.Draft; !d.Printer.Mirror || !d.Printed || d.Printer.Media != "dk-22210" {
		t.Errorf("fixes lost: %+v printed=%v", d.Printer, d.Printed)
	}
}

// A Part-DB on plain http gets a plain explanation, and the wizard can allow it.
func TestPlainHTTPPartDB(t *testing.T) {
	h := newHarness(t)
	w := h.post("/setup/partdb", url.Values{"base_url": {"http://192.0.2.1"}, "token": {"tcp_test"}})
	if body := w.Body.String(); !strings.Contains(body, "https://") || !strings.Contains(body, `name="allow_http"`) {
		t.Errorf("plain http: %s", body)
	}
	wizardToTestWith(t, h, url.Values{"base_url": {partDBWith(t, h, nil)}, "token": {"tcp_test"}, "allow_http": {"on"}})
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"Desk"}})
	if in := h.srv.config().Instances; len(in) != 1 || !in[0].AllowHTTP {
		t.Errorf("allow_http not saved: %+v", in)
	}
}

// The wizard updates the configured instance whatever its id, and starts its jobs.
func TestWizardKeepsInstanceID(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Instances = []config.Instance{{ID: "home", BaseURL: "https://old.example"}}
	var started []string
	h.srv.OnInstance = func(in config.Instance) { started = append(started, in.ID) }
	wizardToTestWith(t, h, url.Values{"base_url": {partDBWith(t, h, nil)}, "token": {"tcp_test"}}, "/setup/partdb?edit=1")
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"Desk"}})
	if len(started) != 1 || started[0] != "home" {
		t.Errorf("jobs started for %v", started)
	}
}

// brokenTransport is a printer that can't be reached.
type brokenTransport struct{}

func (brokenTransport) Open(context.Context) error  { return errors.New("no route to host") }
func (brokenTransport) Write(p []byte) (int, error) { return 0, errors.New("closed") }
func (brokenTransport) Read([]byte) (int, error)    { return 0, errors.New("closed") }
func (brokenTransport) Close() error                { return nil }
func (brokenTransport) Kind() string                { return "tcp" }

// A fix whose reprint fails isn't confirmed by the earlier test: "Looks right" and saving wait
// for a label that printed with the current settings; "Print again" retries without changes.
func TestFailedReprintNeedsNewTest(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	h.srv.Svc.OpenTransport = func(*config.Printer) driver.Transport { return brokenTransport{} }
	body := h.post("/setup/test", url.Values{"action": {"mirrored"}}).Body.String()
	if strings.Contains(body, `value="right"`) {
		t.Errorf("Looks right offered after a failed print: %s", body)
	}
	if !strings.Contains(body, `value="print"`) {
		t.Errorf("no Print again: %s", body)
	}
	wantRedirect(t, h.post("/setup/name", url.Values{"name": {"Desk"}}), "/setup/test")
}

func TestPrintAgainAfterNothingPrinted(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	if body := h.post("/setup/test", url.Values{"action": {"nothing"}}).Body.String(); !strings.Contains(body, `value="print"`) {
		t.Errorf("no Print again after nothing printed: %s", body)
	}
	h.post("/setup/test", url.Values{"action": {"print"}})
	<-h.jobs
}

// A repeated Save (double click, reload) doesn't add the printer twice.
func TestNameSaveOnce(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, nil))
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"Desk"}})
	wantRedirect(t, h.post("/setup/name", url.Values{"name": {"Desk"}}), "/setup/done")
	if n := len(h.srv.config().Printers); n != 2 { // the harness's "ql" + Desk
		t.Errorf("%d printers", n)
	}
}

// A Part-DB whose print agent API can't be checked isn't promised to work.
func TestAgentAPIUnknown(t *testing.T) {
	h := newHarness(t)
	wizardToTest(t, h, partDBWith(t, h, map[string]http.HandlerFunc{"/en/agent-api/": agentAPI(http.StatusInternalServerError)}))
	h.post("/setup/test", url.Values{"action": {"right"}})
	h.post("/setup/name", url.Values{"name": {"Desk"}})
	if body := h.do("GET", "/setup/done", nil).Body.String(); strings.Contains(body, "Part-DB now shows") || !strings.Contains(body, "couldn't check") {
		t.Errorf("done page: %s", body)
	}
}

// A fresh start checks the configured Part-DB again.
func TestFreshStartRechecksAPI(t *testing.T) {
	h := newHarness(t)
	h.srv.local.Draft.APIChecked = true
	h.do("GET", "/setup/", nil)
	if h.srv.local.Draft.APIChecked {
		t.Error("APIChecked survived a fresh start")
	}
}

// When saving fails, the keychain keeps the token that goes with the saved URL.
func TestFailedSaveKeepsKeychainToken(t *testing.T) {
	h := newHarness(t)
	if err := keyring.Set(config.KeyringService, "default", "old-token"); err != nil {
		t.Fatal(err)
	}
	h.srv.CfgPath = t.TempDir() // a directory: writing the config fails
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})
	if got, _ := keyring.Get(config.KeyringService, "default"); got != "old-token" {
		t.Errorf("keychain token %q after a failed save", got)
	}
}

// The keychain undo of a failed save runs before the config lock is released, so a save that
// succeeds meanwhile can't be overwritten by it.
func TestUpdateUndoRunsUnderLock(t *testing.T) {
	h := newHarness(t)
	h.srv.CfgPath = t.TempDir() // saving fails
	var lockedDuringUndo bool
	err := h.srv.updateUndo(func(c *config.Config) (func(), error) {
		return func() {
			lockedDuringUndo = !h.srv.mu.TryLock()
			if !lockedDuringUndo {
				h.srv.mu.Unlock()
			}
		}, nil
	})
	if err == nil || !lockedDuringUndo {
		t.Errorf("err %v, undo under the lock %v", err, lockedDuringUndo)
	}
}

// Step 1 offers to open Part-DB's "new API token" page for the address typed.
func TestPartDBStepLinksToTokenPage(t *testing.T) {
	h := newHarness(t)
	h.do("GET", "/setup/", nil)
	body := h.do("GET", "/setup/partdb", nil).Body.String()
	if !strings.Contains(body, `data-token-page="/en/user/api_token/create"`) || !strings.Contains(body, `src="/static/app.js"`) {
		t.Errorf("no token page button: %s", body)
	}
}

// A long SNMP description is cut to the model part.
func TestCandidateNameShort(t *testing.T) {
	c := discovery.Candidate{SNMPDescr: "Brother HL-L3270CDW series; Brother NC-9400h, Firmware Ver.Z ,MID 84E-755"}
	if got := candidateName(c); got != "Brother HL-L3270CDW series" {
		t.Errorf("name %q", got)
	}
}

// A printer that didn't say what it is (a Zebra without SNMP) can be asked, with a warning.
func TestWizardIdentify(t *testing.T) {
	h := newHarness(t)
	fake := &zpltest.Fake{}
	host, port, _ := net.SplitHostPort(fake.Start(t))
	p, _ := strconv.Atoi(port)
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "tcp", Host: host, Port: p, Status: discovery.Unknown}}, nil
	}
	h.do("GET", "/setup/", nil)
	h.srv.local.Draft.Instance = &config.Instance{ID: "default", BaseURL: h.partdb.URL, Token: "tcp_test"} // step 1 done
	body := h.do("GET", "/setup/printer", nil).Body.String()
	if !strings.Contains(body, "Ask this printer what it is") || !strings.Contains(body, "isn't a Zebra may print") {
		t.Fatalf("no identify button with warning: %s", body)
	}
	if fake.Received() != "" {
		t.Fatal("sent something before the button was pressed")
	}
	wantRedirect(t, h.post("/setup/printer/identify", url.Values{"candidate": {"0"}, "host": {"192.0.2.99"}}), "/setup/printer")
	if fake.Received() != "" {
		t.Fatal("identified a printer the form didn't name")
	}
	wantRedirect(t, h.post("/setup/printer/identify", url.Values{"candidate": {"0"}, "host": {host}}), "/setup/printer")
	if c := h.srv.local.Draft.Candidates[0]; c.ModelID != "GK420d" || c.Status != discovery.Supported {
		t.Errorf("after identify: %+v", c)
	}
}
