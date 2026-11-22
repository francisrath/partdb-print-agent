package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/francisrath/pdb-print/internal/driver/zpl/zpltest"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/discovery"
	"github.com/francisrath/pdb-print/internal/driver"
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
	if w := h.do("GET", "/setup/done", nil); !strings.Contains(w.Body.String(), "Part-DB now shows") ||
		!strings.Contains(w.Body.String(), `name="telemetry" value="1" checked`) {
		t.Errorf("done page: %s", w.Body)
	}
	if h.srv.config().Telemetry != nil {
		t.Fatal("usage report decided before the last step")
	}
	h.post("/setup/done", url.Values{"action": {"finish"}}) // box unticked: the browser sends no field
	if on := h.srv.config().Telemetry; on == nil || *on {
		t.Errorf("unticked box: telemetry %v, want false", on)
	}
	h.post("/setup/done", url.Values{"action": {"finish"}, "telemetry": {"1"}})
	if on := h.srv.config().Telemetry; on == nil || !*on {
		t.Errorf("ticked box: telemetry %v, want true", on)
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
	if w := h.do("GET", "/setup/report", nil); !strings.Contains(w.Body.String(), "github.com/francisrath/pdb-print/issues/new") {
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

// A BLE cat printer is a "Bluetooth" row, and picking it sets up a ble transport.
func TestWizardBLERow(t *testing.T) {
	h := newHarness(t)
	m, _ := devicedb.Default().Model("MX10")
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", BLEName: "MX10", Model: m, ModelID: "MX10", Status: discovery.Supported}}, nil
	}
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "<strong>MX10</strong> · Bluetooth AA:BB:CC:DD:EE:01") {
		t.Fatalf("printer page: %d %s", w.Code, w.Body)
	}
	wantRedirect(t, h.post("/setup/printer", url.Values{"choice": {"0"}}), "/setup/test")
	want := config.Transport{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", Name: "MX10"}
	if p := h.srv.local.Draft.Printer; p.Model != "MX10" || p.Transport != want {
		t.Errorf("picked %+v", p)
	}
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), `value="0" checked`) {
		t.Errorf("the picked Bluetooth printer isn't marked current")
	}
}

// A Bluetooth printer configured by name only is recognised by its advertised name.
func TestWizardBLEExistingByName(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "cat", Name: "Bench cat", Model: "MX10",
		Transport: config.Transport{Kind: "ble", Name: "MX10"}})
	m, _ := devicedb.Default().Model("MX10")
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", BLEName: "MX10", Model: m, ModelID: "MX10", Status: discovery.Supported}}, nil
	}
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "already set up as “Bench cat”") {
		t.Errorf("printer page: %s", w.Body)
	}
}

// A configured Bluetooth printer the wizard's scan found counts as online for Part-DB (the online
// check never connects to a BLE printer).
func TestWizardScanMarksBLEOnline(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "cat", Model: "MX10",
		Transport: config.Transport{Kind: "ble", Name: "MX10"}})
	p := &h.srv.cfg.Printers[len(h.srv.cfg.Printers)-1]
	if h.srv.Svc.Online(p) {
		t.Fatal("online before any scan")
	}
	m, _ := devicedb.Default().Model("MX10")
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", BLEName: "MX10", Model: m, ModelID: "MX10", Status: discovery.Supported}}, nil
	}
	h.do("GET", "/setup/printer", nil)
	if !h.srv.Svc.Online(p) {
		t.Error("offline after the scan found it")
	}
}

// A Bluetooth Classic printer is a "Bluetooth" row, and picking it sets up a bt transport.
func TestWizardBTRow(t *testing.T) {
	h := newHarness(t)
	m, _ := devicedb.Default().Model("RG-MLP80B")
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "bt", Address: "AA:BB:CC:DD:EE:01", BTName: "RG-MLP80B-1", Model: m, ModelID: "RG-MLP80B", Status: discovery.Supported}}, nil
	}
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), "<strong>RG-MLP80B</strong> · Bluetooth AA:BB:CC:DD:EE:01") {
		t.Fatalf("printer page: %d %s", w.Code, w.Body)
	}
	wantRedirect(t, h.post("/setup/printer", url.Values{"choice": {"0"}}), "/setup/test")
	want := config.Transport{Kind: "bt", Address: "AA:BB:CC:DD:EE:01", Name: "RG-MLP80B-1"}
	if p := h.srv.local.Draft.Printer; p.Model != "RG-MLP80B" || p.Transport != want {
		t.Errorf("picked %+v", p)
	}
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), `value="0" checked`) {
		t.Errorf("the picked Bluetooth printer isn't marked current")
	}
	h.srv.local.Draft.Printer.Transport.Address = "aa:bb:cc:dd:ee:01" // config doesn't normalise the case
	if w := h.do("GET", "/setup/printer", nil); !strings.Contains(w.Body.String(), `value="0" checked`) {
		t.Errorf("the picked Bluetooth printer (lower-case address) isn't marked current")
	}
}

// A configured bt printer is recognised by its address (any case) or, configured by name only, by its
// name; a ble printer of the same name is not the bt one.
func TestWizardBTExisting(t *testing.T) {
	m, _ := devicedb.Default().Model("RG-MLP80B")
	for _, tc := range []struct {
		name string
		t    config.Transport
		want bool
	}{
		{"address", config.Transport{Kind: "bt", Address: "aa:bb:cc:dd:ee:01"}, true},
		{"name", config.Transport{Kind: "bt", Name: "RG-MLP80B-1"}, true},
		{"other address", config.Transport{Kind: "bt", Address: "AA:BB:CC:DD:EE:02", Name: "RG-MLP80B-1"}, false},
		{"ble of that name", config.Transport{Kind: "ble", Name: "RG-MLP80B-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "rego", Name: "Bench receipt", Model: "RG-MLP80B", Transport: tc.t})
			h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
				return []discovery.Candidate{{Kind: "bt", Address: "AA:BB:CC:DD:EE:01", BTName: "RG-MLP80B-1", Model: m, ModelID: "RG-MLP80B", Status: discovery.Supported}}, nil
			}
			w := h.do("GET", "/setup/printer", nil)
			if got := strings.Contains(w.Body.String(), "already set up as “Bench receipt”"); got != tc.want {
				t.Errorf("existing %v, want %v: %s", got, tc.want, w.Body)
			}
		})
	}
}

// Unlike a BLE advert, a classic scan also lists paired printers that are switched off: finding one
// doesn't make it online.
func TestWizardScanLeavesBTOffline(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Printers = append(h.srv.cfg.Printers, config.Printer{ID: "rego", Model: "RG-MLP80B",
		Transport: config.Transport{Kind: "bt", Address: "AA:BB:CC:DD:EE:01"}})
	p := &h.srv.cfg.Printers[len(h.srv.cfg.Printers)-1]
	m, _ := devicedb.Default().Model("RG-MLP80B")
	h.srv.Discover = func(ctx context.Context, ip string) ([]discovery.Candidate, error) {
		return []discovery.Candidate{{Kind: "bt", Address: "AA:BB:CC:DD:EE:01", BTName: "RG-MLP80B", Model: m, ModelID: "RG-MLP80B", Status: discovery.Supported}}, nil
	}
	h.do("GET", "/setup/printer", nil)
	if h.srv.Svc.Online(p) {
		t.Error("online after a scan listed it")
	}
}

// pairLoopbackHarness is a harness in service mode: loopback needs a paired session too.
func pairLoopbackHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.srv.PairLoopback = true
	h.srv.SessionKeyFile = filepath.Join(t.TempDir(), "session.key")
	h.srv.PairRequestFile = filepath.Join(t.TempDir(), "pair-request")
	h.h = h.srv.Handler()
	return h
}

// pairRequest writes a nonce to the request file, as `pdb-print pair` does, and returns a request
// option that sends it.
func pairRequest(t *testing.T, h *harness, nonce string) func(*http.Request) {
	t.Helper()
	if err := os.WriteFile(h.srv.PairRequestFile, []byte(nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set(PairRequestHeader, nonce) }
}

func fromLoopback(r *http.Request) { r.RemoteAddr = "127.0.0.1:50000" }

func TestPairLoopbackIndexNeedsSession(t *testing.T) {
	h := pairLoopbackHarness(t)
	for _, tc := range []struct {
		method, path string
		want         int // a redirect goes to the pair page
	}{
		{"GET", "/", http.StatusSeeOther},
		{"GET", "/?manual", http.StatusSeeOther},
		{"GET", "/setup/partdb", http.StatusSeeOther},
		{"GET", "/setup/done", http.StatusSeeOther},
		{"GET", "/printers/ql/status", http.StatusSeeOther},
		{"POST", "/routes", http.StatusForbidden},
		{"POST", "/instance", http.StatusForbidden},
		{"POST", "/telemetry", http.StatusForbidden},
		{"POST", "/printers/ql/test", http.StatusForbidden},
		{"POST", "/setup/partdb", http.StatusForbidden},
		{"POST", "/v1/quit", http.StatusForbidden},
		// open without pairing
		{"GET", "/setup/", http.StatusOK}, // the pair page
		{"GET", "/v1/info", http.StatusOK},
		{"GET", "/v1/handoff", http.StatusOK}, // its own auth: the Part-DB pairing key
		{"GET", "/static/style.css", http.StatusOK},
		{"GET", "/favicon.svg", http.StatusOK},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var form url.Values
			if tc.method == "POST" {
				form = url.Values{"csrf": {h.srv.csrf}}
			}
			w := h.do(tc.method, tc.path, form, fromLoopback, func(r *http.Request) { r.Header.Set("X-PDB-Print", "1") })
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if tc.want == http.StatusSeeOther && w.Header().Get("Location") != "/setup/" {
				t.Errorf("redirect to %q, want the pair page", w.Header().Get("Location"))
			}
			if tc.path == "/setup/" && !strings.Contains(w.Body.String(), "Setup is closed") {
				t.Errorf("not the pair page: %s", w.Body)
			}
		})
	}
}

// pairLoopback pairs a loopback browser with the window's code and returns its session cookie.
func pairLoopback(t *testing.T, h *harness) *http.Cookie {
	t.Helper()
	code, _ := h.srv.Window.Open(time.Now())
	w := h.do("POST", "/setup/pair", url.Values{"code": {code}}, fromLoopback)
	wantRedirect(t, w, "/setup/partdb")
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie: %v", w.Result().Cookies())
	return nil
}

// A session key file not owned by Administrators/SYSTEM (planted) signs nothing: its cookies are
// refused, and the next pairing replaces it.
func TestPairLoopbackUntrustedKey(t *testing.T) {
	h := pairLoopbackHarness(t)
	cookie := pairLoopback(t, h)
	withCookie := func(r *http.Request) { r.AddCookie(cookie) }
	if w := h.do("GET", "/", nil, fromLoopback, withCookie); w.Code != http.StatusOK {
		t.Fatalf("paired: %d", w.Code)
	}
	owned := false
	oldCheck := keyOwnerOK
	keyOwnerOK = func(string) (bool, error) { return owned, nil }
	t.Cleanup(func() { keyOwnerOK = oldCheck })
	if w := h.do("GET", "/", nil, fromLoopback, withCookie); w.Code != http.StatusSeeOther {
		t.Errorf("planted key accepted: %d", w.Code)
	}
	old, _ := os.ReadFile(h.srv.SessionKeyFile)
	fresh := pairLoopback(t, h) // replaces the key
	if b, _ := os.ReadFile(h.srv.SessionKeyFile); len(b) != 32 || string(b) == string(old) {
		t.Errorf("key not replaced")
	}
	owned = true
	if w := h.do("GET", "/", nil, fromLoopback, func(r *http.Request) { r.AddCookie(fresh) }); w.Code != http.StatusOK {
		t.Errorf("new session: %d", w.Code)
	}
	if w := h.do("GET", "/", nil, fromLoopback, withCookie); w.Code != http.StatusSeeOther {
		t.Errorf("old cookie after the key changed: %d", w.Code)
	}
}

func TestPairLoopbackPairs(t *testing.T) {
	h := pairLoopbackHarness(t)
	code, _ := h.srv.Window.Open(time.Now())
	if w := h.do("GET", "/setup/", nil, fromLoopback); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "6-digit code") {
		t.Fatalf("pair page: %d %s", w.Code, w.Body)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if w := h.do("POST", "/setup/pair", url.Values{"code": {wrong}}, fromLoopback); w.Code != http.StatusForbidden {
		t.Fatalf("wrong code: %d", w.Code)
	}
	cookie := pairLoopback(t, h)
	if cookie.Path != "/" || cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.MaxAge != int(sessionLife/time.Second) {
		t.Fatalf("session cookie %+v", cookie)
	}
	if fi, err := os.Stat(h.srv.SessionKeyFile); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	withCookie := func(r *http.Request) { r.AddCookie(cookie); fromLoopback(r) }
	h.srv.Window.Close() // the session outlives the window
	if w := h.do("GET", "/", nil, withCookie); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), h.srv.csrf) {
		t.Fatalf("index after pairing: %d %s", w.Code, w.Body)
	}
	if w := h.do("GET", "/setup/partdb", nil, withCookie); w.Code != http.StatusOK {
		t.Fatalf("wizard after pairing: %d %s", w.Code, w.Body)
	}
	if w := h.do("POST", "/routes", url.Values{"csrf": {h.srv.csrf}, "profile": {"2"}, "printer": {"ql"}}, withCookie); w.Code != http.StatusSeeOther {
		t.Fatalf("settings POST after pairing: %d %s", w.Code, w.Body)
	}
	if w := h.do("POST", "/routes", url.Values{"profile": {"2"}, "printer": {"ql"}}, withCookie); w.Code != http.StatusForbidden {
		t.Errorf("settings POST with the cookie but no CSRF token: %d", w.Code)
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	h := pairLoopbackHarness(t)
	cookie := pairLoopback(t, h)
	h2 := newHarness(t) // a fresh Server: new secrets in memory, the same key file
	h2.srv.PairLoopback, h2.srv.SessionKeyFile = true, h.srv.SessionKeyFile
	h2.h = h2.srv.Handler()
	if w := h2.do("GET", "/", nil, func(r *http.Request) { r.AddCookie(cookie) }); w.Code != http.StatusOK {
		t.Errorf("after a restart: %d", w.Code)
	}
}

func TestSessionCookieRejected(t *testing.T) {
	h := pairLoopbackHarness(t)
	good := pairLoopback(t, h).Value
	expired, err := h.srv.signSession(time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	exp, sig, _ := strings.Cut(good, ".")
	later, _ := strconv.ParseInt(exp, 10, 64)
	flip := map[byte]byte{'0': '1'}[sig[len(sig)-1]]
	if flip == 0 {
		flip = '0'
	}
	for _, tc := range []struct{ name, value string }{
		{"tampered signature", exp + "." + sig[:len(sig)-1] + string(flip)},
		{"extended expiry", strconv.FormatInt(later+86400, 10) + "." + sig},
		{"expired", expired},
		{"no signature", exp},
		{"garbage", "x.y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do("GET", "/", nil, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.value}) })
			if w.Code != http.StatusSeeOther {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	t.Run("key file deleted", func(t *testing.T) {
		if err := os.Remove(h.srv.SessionKeyFile); err != nil {
			t.Fatal(err)
		}
		w := h.do("GET", "/", nil, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: good}) })
		if w.Code != http.StatusSeeOther {
			t.Errorf("status %d", w.Code)
		}
	})
}

func TestNoLongSessionByDefault(t *testing.T) {
	h := newHarness(t)
	h.srv.SessionKeyFile = filepath.Join(t.TempDir(), "session.key") // set, but PairLoopback is off
	code, _, _ := h.srv.OpenWindow(time.Now())
	w := serveLAN(h.srv.LANHandler(), lanReq("POST", "/setup/pair", url.Values{"code": {code}}, nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("LAN pairing: %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Errorf("long session issued on the LAN: %+v", c)
		}
	}
	if _, err := os.Stat(h.srv.SessionKeyFile); err == nil {
		t.Error("key file created outside PairLoopback")
	}
}

// pairCode posts /v1/pair and returns the code (from the reply or its code file) and the expiry.
func pairCode(t *testing.T, h *harness) (string, time.Time) {
	t.Helper()
	opts := []func(*http.Request){fromLoopback}
	if h.srv.PairLoopback {
		opts = append(opts, pairRequest(t, h, randomHex(32)))
	}
	w := h.do("POST", "/v1/pair", url.Values{}, opts...)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res struct {
		Code     string    `json:"code"`
		CodeFile string    `json:"code_file"`
		Expires  time.Time `json:"expires"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.CodeFile != "" {
		b, err := os.ReadFile(res.CodeFile)
		if err != nil {
			t.Fatal(err)
		}
		res.Code = string(b)
	}
	return res.Code, res.Expires
}

func TestPairAPIReusesOpenWindow(t *testing.T) {
	for _, codeFile := range []bool{false, true} {
		t.Run(fmt.Sprint("code file ", codeFile), func(t *testing.T) {
			h := newHarness(t)
			if codeFile {
				h = pairLoopbackHarness(t)
				h.srv.PairCodeFile = filepath.Join(t.TempDir(), "pair-code.txt")
			}
			code, expires := pairCode(t, h)
			h.srv.Window.Check("wrong", time.Now())
			again, expires2 := pairCode(t, h)
			if again != code || !expires2.Equal(expires) {
				t.Errorf("second /v1/pair: %q %v, want %q %v", again, expires2, code, expires)
			}
			h.srv.Window.mu.Lock()
			tries := h.srv.Window.tries
			h.srv.Window.mu.Unlock()
			if tries != 1 {
				t.Errorf("tries %d after a reuse, want 1", tries)
			}
			if _, ok := h.srv.Window.Check(code, time.Now()); !ok { // a browser pairs: the code is used up
				t.Fatal("code didn't pair")
			}
			h.srv.Window.mu.Lock()
			h.srv.Window.opened = h.srv.Window.opened.Add(-windowEvery)
			h.srv.Window.mu.Unlock()
			if fresh, _ := pairCode(t, h); fresh == code {
				t.Errorf("/v1/pair after the code was used: the same code %q", fresh)
			}
		})
	}
}

func TestPairAPIOncePerMinute(t *testing.T) {
	h := newHarness(t)
	pairCode(t, h)
	h.srv.Window.Close() // e.g. 5 wrong codes
	w := h.do("POST", "/v1/pair", url.Values{}, fromLoopback)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "minute") {
		t.Fatalf("new window within a minute: %d %s", w.Code, w.Body)
	}
	h.srv.Window.mu.Lock()
	h.srv.Window.opened = h.srv.Window.opened.Add(-time.Minute)
	h.srv.Window.mu.Unlock()
	if again, _ := pairCode(t, h); again == "" { // a new window is allowed again
		t.Errorf("a minute later: %q", again)
	}
}

func TestPairCodeFile(t *testing.T) {
	h := pairLoopbackHarness(t)
	h.srv.PairCodeFile = filepath.Join(t.TempDir(), "pair-code.txt")
	w := h.do("POST", "/v1/pair", url.Values{}, fromLoopback, pairRequest(t, h, "ab12"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if _, ok := res["code"]; ok || res["code_file"] != h.srv.PairCodeFile {
		t.Fatalf("reply %s", w.Body)
	}
	b, err := os.ReadFile(h.srv.PairCodeFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.srv.Window.Check(strings.TrimSpace(string(b)), time.Now()); !ok {
		t.Errorf("file holds %q, not the window's code", b)
	}
}

func TestDefaultLoopbackUnchanged(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/", nil); w.Code != http.StatusOK {
		t.Errorf("index: %d", w.Code)
	}
	wantRedirect(t, h.do("GET", "/setup/", nil), "/setup/partdb")
	if w := h.do("GET", "/setup/partdb", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Set up by hand") {
		t.Errorf("wizard: %d", w.Code)
	}
	if w := h.do("POST", "/setup/pair", url.Values{"csrf": {h.srv.csrf}, "code": {"123456"}}); w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
		t.Errorf("POST /setup/pair on loopback: %d", w.Code)
	}
	w := h.do("POST", "/v1/pair", url.Values{}, fromLoopback)
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res["code"] == nil || res["code_file"] != nil {
		t.Errorf("pair reply %s: %v", w.Body, err)
	}
}

// Under PairLoopback only an administrator opens a window: the request needs the nonce from the
// admin-only request file, once.
func TestPairLoopbackNeedsRequest(t *testing.T) {
	h := pairLoopbackHarness(t)
	h.srv.PairCodeFile = filepath.Join(t.TempDir(), "pair-code.txt")
	refused := func(what string, opts ...func(*http.Request)) {
		t.Helper()
		w := h.do("POST", "/v1/pair", url.Values{}, append([]func(*http.Request){fromLoopback}, opts...)...)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "run pdb-print pair as administrator") {
			t.Errorf("%s: %d %s", what, w.Code, w.Body)
		}
		if h.srv.Window.IsOpen(time.Now()) {
			t.Fatalf("%s: window opened", what)
		}
	}
	refused("no request file")
	refused("no file, empty header", func(r *http.Request) { r.Header.Set(PairRequestHeader, "") })
	pairRequest(t, h, "c0ffee")
	refused("no header")
	refused("wrong nonce", func(r *http.Request) { r.Header.Set(PairRequestHeader, "c0ffef") })
	ok := func(r *http.Request) { r.Header.Set(PairRequestHeader, "c0ffee") }
	if w := h.do("POST", "/v1/pair", url.Values{}, fromLoopback, ok); w.Code != http.StatusOK {
		t.Fatalf("right nonce: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(h.srv.PairRequestFile); !os.IsNotExist(err) {
		t.Errorf("request file kept: %v", err)
	}
	h.srv.Window.Close()
	h.srv.Window.mu.Lock()
	h.srv.Window.opened = h.srv.Window.opened.Add(-windowEvery) // not the once-a-minute limit
	h.srv.Window.mu.Unlock()
	refused("nonce used twice", ok)
}

func TestPairLoopbackNeedsCodeFile(t *testing.T) {
	h := pairLoopbackHarness(t) // no PairCodeFile: the code must not go to any local user
	w := h.do("POST", "/v1/pair", url.Values{}, fromLoopback)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "no code file") || strings.Contains(w.Body.String(), `"code"`) {
		t.Errorf("status %d: %s", w.Code, w.Body)
	}
	if h.srv.Window.IsOpen(time.Now()) {
		t.Error("window opened without a code file")
	}
}

func TestHandoffOwnToken(t *testing.T) {
	h := newHarness(t)
	h.post("/instance", url.Values{"base_url": {h.partdb.URL}, "token": {"tcp_test"}})
	h.post("/routes", url.Values{"profile": {"2"}, "printer": {"ql"}})
	h.srv.PairLoopback, h.srv.SessionKeyFile = true, filepath.Join(t.TempDir(), "session.key")
	h.h = h.srv.Handler()
	w := h.do("GET", "/v1/handoff?src="+url.QueryEscape(h.partdb.URL)+"&type=part&ids=32&profile=2", nil) // unpaired
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Print this label?") ||
		strings.Contains(w.Body.String(), h.srv.csrf) || !strings.Contains(w.Body.String(), h.srv.handoffCSRF) {
		t.Fatalf("handoff page: %d, settings token shown or own token missing: %s", w.Code, w.Body)
	}
	form := url.Values{"src": {h.partdb.URL}, "type": {"part"}, "ids": {"32"}, "profile": {"2"}}
	form.Set("csrf", h.srv.csrf)
	if w := h.do("POST", "/v1/handoff", form); w.Code != http.StatusForbidden {
		t.Errorf("handoff POST with the settings token: %d", w.Code)
	}
	form.Set("csrf", h.srv.handoffCSRF)
	if w := h.do("POST", "/v1/handoff", form); !strings.Contains(w.Body.String(), "Printed ✓") {
		t.Fatalf("handoff POST with its own token, unpaired: %d %s", w.Code, w.Body)
	}
	<-h.jobs
	if w := h.do("POST", "/v1/handoff", url.Values{"csrf": {h.srv.csrf}}); w.Code != http.StatusForbidden {
		t.Errorf("handoff POST with the settings token: %d", w.Code)
	}
	cookie := pairLoopback(t, h)
	if w := h.do("POST", "/routes", url.Values{"csrf": {h.srv.handoffCSRF}, "profile": {"2"}, "printer": {"ql"}},
		func(r *http.Request) { r.AddCookie(cookie) }); w.Code != http.StatusForbidden {
		t.Errorf("settings POST with the handoff token: %d", w.Code)
	}
}

func TestNewCodeKeepsPairedBrowser(t *testing.T) {
	h := newHarness(t)
	lan := h.srv.LANHandler()
	code, _ := pairCode(t, h)
	a := pairLAN(lan, code)
	if a == nil {
		t.Fatal("browser A didn't pair")
	}
	h.srv.Window.mu.Lock()
	h.srv.Window.opened = h.srv.Window.opened.Add(-windowEvery)
	h.srv.Window.mu.Unlock()
	code2, _ := pairCode(t, h) // `pdb-print pair` for browser B
	if code2 == code {
		t.Fatal("no new code")
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/partdb", nil, a)); w.Code != http.StatusOK {
		t.Errorf("browser A after the new code: %d %s", w.Code, w.Header().Get("Location"))
	}
	if b := pairLAN(lan, code2); b == nil {
		t.Error("browser B didn't pair with the new code")
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/partdb", nil, a)); w.Code != http.StatusOK {
		t.Errorf("browser A after B paired: %d", w.Code)
	}
}
