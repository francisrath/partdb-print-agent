package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/report"
)

// Draft is a wizard session's setup until the printer is named and saved.
type Draft struct {
	mu sync.Mutex // one request per session at a time

	Instance   *config.Instance // a new Part-DB connection (token included); nil = keep the configured one
	Notes      []string         // shown on the printer page (connected as …, warnings)
	NoAgentAPI bool             // Part-DB can't send jobs to this agent
	APIErr     string           // the print agent API couldn't be checked (unknown)
	APIChecked bool             // NoAgentAPI is known for the configured instance

	Candidates []discovery.Candidate
	Scanned    bool

	Printer       config.Printer // the chosen printer (ID "setup" until named)
	Printed       bool           // a test label came out
	MediaReported bool           // the printer reports its roll
	AskMedia      bool           // show the tape picker
	Msg           string         // status or hint after a fault picture
	ShowReport    bool
	FixRounds     int

	SavedCurrent bool     // this printer is saved (a repeated Save goes to Done)
	Saved        []string // names saved in this session
}

// resetPrinter starts the printer steps over with p.
func (d *Draft) resetPrinter(p config.Printer) {
	d.Printer, d.Printed, d.MediaReported, d.AskMedia, d.Msg, d.ShowReport, d.FixRounds = p, false, false, false, "", false, 0
	d.SavedCurrent = false
}

const issueURL = "https://github.com/francisrath/partdb-print-agent/issues/new"

// setupRoutes registers the wizard's pages on mux (LAN and loopback).
func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup/partdb", s.setupPartDB)
	mux.HandleFunc("POST /setup/partdb", s.setupPartDBPost)
	mux.HandleFunc("GET /setup/printer", s.setupPrinter)
	mux.HandleFunc("POST /setup/printer", s.setupPrinterPost)
	mux.HandleFunc("POST /setup/printer/probe", s.setupProbe)
	mux.HandleFunc("POST /setup/printer/identify", s.setupIdentify)
	mux.HandleFunc("GET /setup/test", s.setupTest)
	mux.HandleFunc("POST /setup/test", s.setupTestPost)
	mux.HandleFunc("GET /setup/name", s.setupName)
	mux.HandleFunc("POST /setup/name", s.setupNamePost)
	mux.HandleFunc("GET /setup/done", s.setupDone)
	mux.HandleFunc("POST /setup/done", s.setupDonePost)
	mux.HandleFunc("GET /setup/report", s.setupReport)
	mux.HandleFunc("POST /setup/report", s.setupReportPost)
}

// withLocalSession gives loopback wizard requests the one local session (no code needed here).
func (s *Server) withLocalSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSetupPath(r.URL.Path) {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, s.local))
		}
		next.ServeHTTP(w, r)
	})
}

// lockDraft serialises a session's requests; the returned draft is valid until unlock.
func lockDraft(r *http.Request) (*Session, *Draft, func()) {
	sess := session(r)
	d := sess.Draft
	d.mu.Lock()
	return sess, d, d.mu.Unlock
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request, page string, step int, data map[string]any) {
	sess := session(r)
	if data == nil {
		data = map[string]any{}
	}
	data["Step"], data["CSRF"], data["D"], data["Version"] = step, sess.CSRF, sess.Draft, s.Version
	data["Local"] = sess == s.local
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, page, data); err != nil {
		s.Log.Error("setup page", "page", page, "err", err)
	}
}

// --- step 1: Part-DB

func (s *Server) setupPartDB(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	cur, err := s.config().Instance("")
	if d.Instance == nil && err == nil && r.URL.Query().Get("edit") == "" {
		// already connected: check once whether it can send jobs, then on to the printer
		if !d.APIChecked {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			ok, err := partdb.FromInstance(cur).HasAgentAPI(ctx)
			cancel()
			d.NoAgentAPI, d.APIErr, d.APIChecked = err == nil && !ok, errText(err), true
		}
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	base := ""
	if d.Instance != nil {
		base = d.Instance.BaseURL
	} else if err == nil {
		base = cur.BaseURL
	}
	s.setupPage(w, r, "setup-partdb", 1, map[string]any{"BaseURL": base})
}

func (s *Server) setupPartDBPost(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	in := config.Instance{ID: "default", BaseURL: strings.TrimRight(strings.TrimSpace(r.FormValue("base_url")), "/"),
		Token: strings.TrimSpace(r.FormValue("token")), AllowHTTP: r.FormValue("allow_http") != ""}
	fail := func(msg string) {
		s.setupPage(w, r, "setup-partdb", 1, map[string]any{"BaseURL": in.BaseURL, "AllowHTTP": in.AllowHTTP, "Err": msg})
	}
	if in.BaseURL == "" || in.Token == "" {
		fail("Enter the address of your Part-DB and an API token.")
		return
	}
	if !strings.Contains(in.BaseURL, "://") {
		in.BaseURL = "https://" + in.BaseURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c := partdb.FromInstance(&in)
	tok, err := c.TokenInfo(ctx)
	if err != nil {
		fail(partDBError(err))
		return
	}
	api, apiErr := c.HasAgentAPI(ctx)
	d.Instance, d.NoAgentAPI, d.APIErr, d.APIChecked = &in, apiErr == nil && !api, errText(apiErr), true
	d.Notes = []string{fmt.Sprintf("Connected to Part-DB as %s.", tok.User.Name)}
	if tok.Level > 1 {
		d.Notes = append(d.Notes, "This token can change data in Part-DB; a read-only token is enough. You can carry on, or create a read-only one and come back.")
	}
	http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// partDBError says in plain words why Part-DB couldn't be reached.
func partDBError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "refusing to send the API token"):
		return "This address starts with http://, so the token would travel unencrypted. Use your Part-DB's https:// address; if it really only has http://, tick “Allow plain http”."
	case strings.Contains(msg, "401") || strings.Contains(msg, "403"):
		return "Part-DB didn't accept the token. Copy it again from Part-DB: your user name (top right) → User settings → API tokens."
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "Couldn't reach Part-DB at that address. Check it opens in a browser on this network. (" + msg + ")"
	}
	return "That doesn't look like Part-DB: " + msg
}

// --- step 2: printer

func (s *Server) setupPrinter(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	var scanErr, bleNote string
	if !d.Scanned || r.URL.Query().Get("rescan") != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		cs, note, err := s.discover(ctx, "")
		cancel()
		if err != nil && len(cs) == 0 {
			scanErr = err.Error()
		}
		bleNote = note
		s.seenBluetooth(cs)
		d.Candidates, d.Scanned = sortCandidates(cs), true
	}
	type row struct {
		Index     int
		C         discovery.Candidate
		Supported bool
		Where     string
		Existing  string // name of a configured printer at this address
		Current   bool   // the printer this session picked
		Name      string
	}
	var rows []row
	for i, c := range d.Candidates {
		cur := d.Printer.Model != "" && c.ModelID == d.Printer.Model &&
			((c.Kind == "tcp" && d.Printer.Transport.Host == c.Host) || (d.Printer.Transport.Address != "" && btSame(d.Printer.Transport, c)) || (c.Kind == "usb" && d.Printer.Transport.Kind == "usb" && d.Printer.Transport.USBID == c.USBID && d.Printer.Transport.Serial == c.Serial && (c.USBID != "" || d.Printer.Transport.Device == c.Device)))
		rows = append(rows, row{Index: i, C: c, Supported: c.Status == discovery.Supported, Where: where(c), Existing: s.existing(c), Current: cur, Name: c.Name()})
	}
	s.setupPage(w, r, "setup-printer", 2, map[string]any{"Rows": rows, "ScanErr": scanErr, "BLENote": bleNote})
}

func sortCandidates(cs []discovery.Candidate) []discovery.Candidate {
	rank := func(c discovery.Candidate) int {
		if c.Status == discovery.Supported {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(cs, func(a, b discovery.Candidate) int { return cmp.Compare(rank(a), rank(b)) })
	return cs
}

func where(c discovery.Candidate) string {
	switch {
	case c.Kind == "usb":
		return "USB"
	case config.Bluetooth(c.Kind):
		return "Bluetooth " + c.Address
	}
	return "network " + c.Host
}

// existing names the configured printer at c's address, if any.
func (s *Server) existing(c discovery.Candidate) string {
	for _, p := range s.config().Printers {
		t := p.Transport
		if (c.Kind == "tcp" && t.Kind == "tcp" && t.Host == c.Host) || btSame(t, c) ||
			(c.Kind == "usb" && t.Kind == "usb" && ((t.USBID != "" && t.USBID == c.USBID && t.Serial == c.Serial) || (t.Device != "" && t.Device == c.Device))) {
			return cmp.Or(p.Name, p.ID)
		}
	}
	return ""
}

// seenBluetooth tells the print service which configured Bluetooth printers a scan found: they count as
// online for a while (see printing.Service.SeenBluetooth).
func (s *Server) seenBluetooth(cs []discovery.Candidate) {
	ps := s.config().Printers
	for _, c := range cs {
		for i := range ps {
			// a classic scan also lists paired printers that are off: only an LE advert means it's there
			if c.Kind == "ble" && btSame(ps[i].Transport, c) {
				s.Svc.SeenBluetooth(&ps[i])
			}
		}
	}
}

// btSame: a Bluetooth printer of c's kind where the address wins (any case: config doesn't normalise it);
// a printer configured by name only matches by the advertised or device name.
func btSame(t config.Transport, c discovery.Candidate) bool {
	if !config.Bluetooth(c.Kind) || t.Kind != c.Kind {
		return false
	}
	if t.Address != "" {
		return strings.EqualFold(t.Address, c.Address)
	}
	return t.Name != "" && t.Name == cmp.Or(c.BLEName, c.BTName)
}

// discover finds printers at ip, or ("") on USB, the LAN and Bluetooth; note says why Bluetooth
// couldn't be searched (no adapter, Bluetooth off), which isn't an error.
func (s *Server) discover(ctx context.Context, ip string) (cs []discovery.Candidate, note string, err error) {
	if s.Discover != nil {
		cs, err = s.Discover(ctx, ip)
		return cs, "", err
	}
	db := devicedb.Default()
	if ip != "" {
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.Unmap().Is4() {
			return nil, "", errors.New("enter the printer's IPv4 address, like 192.168.1.50")
		}
		cs, err = discovery.Network(ctx, db, discovery.Options{Subnets: []netip.Prefix{netip.PrefixFrom(a.Unmap(), 32)}})
		return cs, "", err
	}
	var bt []discovery.Candidate
	btDone := make(chan struct{})
	go func() {
		defer close(btDone)
		bt, note, _ = discovery.Bluetooth(ctx, db, discovery.Options{})
	}()
	usb, _ := discovery.USB(db, "") // not on Linux: no USB printers, not an error here
	lan, err := discovery.Network(ctx, db, discovery.Options{Timeout: 15 * time.Second})
	<-btDone
	return append(append(usb, lan...), bt...), note, err
}

func (s *Server) setupPrinterPost(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	i, err := strconv.Atoi(r.FormValue("choice"))
	if err != nil || i < 0 || i >= len(d.Candidates) || d.Candidates[i].Status != discovery.Supported {
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	c := d.Candidates[i]
	p := config.Printer{ID: "setup", Model: c.ModelID}
	switch {
	case c.Kind == "tcp":
		p.Transport = config.Transport{Kind: "tcp", Host: c.Host, Port: c.Port}
	case config.Bluetooth(c.Kind):
		p.Transport = config.Transport{Kind: c.Kind, Address: c.Address, Name: cmp.Or(c.BLEName, c.BTName)}
	case c.USBID != "":
		p.Transport = config.Transport{Kind: "usb", USBID: c.USBID, Serial: c.Serial}
	default:
		p.Transport = config.Transport{Kind: "usb", Device: c.Device}
	}
	if d.Printer.Model != p.Model || d.Printer.Transport != p.Transport { // Back and Next keep the test print and fixes
		d.resetPrinter(p)
	}
	http.Redirect(w, r, "/setup/test", http.StatusSeeOther)
}

func (s *Server) setupProbe(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	ip := strings.TrimSpace(r.FormValue("ip"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cs, _, err := s.discover(ctx, ip)
	if err == nil && len(cs) == 0 {
		err = fmt.Errorf("nothing answered at %s on the printer port (9100); check the address and that the printer is on", ip)
	}
	if err != nil {
		d.Notes = []string{err.Error()}
	}
	for _, c := range cs {
		if !slices.ContainsFunc(d.Candidates, func(o discovery.Candidate) bool { return o.Kind == c.Kind && o.Host == c.Host }) {
			d.Candidates = append(d.Candidates, c)
		}
	}
	d.Candidates = sortCandidates(d.Candidates)
	http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
}

// setupIdentify asks an unknown network printer what it is (Zebra ~HI), after the user pressed
// the button that warns about it.
func (s *Server) setupIdentify(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	i, err := strconv.Atoi(r.FormValue("candidate"))
	// the host too: the list may have changed since the page was shown, and the consent was for this printer
	if err == nil && i >= 0 && i < len(d.Candidates) && d.Candidates[i].Kind == "tcp" && d.Candidates[i].Host == r.FormValue("host") {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		c, _, err := discovery.Identify(ctx, devicedb.Default(), d.Candidates[i])
		cancel()
		d.Candidates[i] = c
		if err != nil {
			d.Notes = []string{fmt.Sprintf("%s: %v", c.Host, err)}
		}
		d.Candidates = sortCandidates(d.Candidates)
	}
	http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
}

// --- step 3: test print

func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	if d.Printer.Model == "" {
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	s.testPage(w, r)
}

func (s *Server) testPage(w http.ResponseWriter, r *http.Request) {
	var media []devicedb.Media
	if m, ok := devicedb.Default().Model(session(r).Draft.Printer.Model); ok {
		media = devicedb.Default().MediaFor(m)
	}
	s.setupPage(w, r, "setup-test", 3, map[string]any{"Media": media})
}

func (s *Server) setupTestPost(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	if d.Printer.Model == "" {
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	p := &d.Printer
	d.Msg = ""
	switch r.FormValue("action") {
	case "print":
		s.setupPrint(r.Context(), d)
	case "media":
		if _, ok := printing.LookupMedia(p, r.FormValue("media")); ok {
			p.Media = r.FormValue("media")
			d.AskMedia = false
			s.setupPrint(r.Context(), d)
		}
	case "right":
		if d.Printed {
			http.Redirect(w, r, "/setup/name", http.StatusSeeOther)
			return
		}
	case "mirrored":
		p.Mirror = !p.Mirror
		d.FixRounds++
		s.setupPrint(r.Context(), d)
	case "upside-down":
		p.UpsideDown = !p.UpsideDown
		d.FixRounds++
		s.setupPrint(r.Context(), d)
	case "cut-off":
		d.FixRounds++
		if d.MediaReported {
			// the card is sized for the roll the printer reports, so no setting fixes this
			d.Msg = s.statusLine(r.Context(), d) + " Check the roll sits snugly against the guide and the cover is fully closed."
			d.ShowReport = true
		} else {
			d.AskMedia = true
			d.Msg = "Labels come out cut off when the printer is set for another size."
		}
	case "faint":
		d.FixRounds++
		d.Msg = s.statusLine(r.Context(), d) + " Faint prints usually come from an old or non-genuine roll, or a dirty print head: try a fresh roll."
		d.ShowReport = true
	case "nothing":
		d.FixRounds++
		d.Msg = s.statusLine(r.Context(), d) + " Check the printer is on, a roll is loaded and the cover is closed."
		d.ShowReport = true
	}
	if d.FixRounds >= 3 {
		d.ShowReport = true
	}
	s.testPage(w, r)
}

// setupPrint prints the test card with the draft's settings; a printer that doesn't report its
// roll is asked about first.
func (s *Server) setupPrint(ctx context.Context, d *Draft) {
	p := &d.Printer
	d.Printed = false // only a label printed with the current settings counts
	if p.Media == "" {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := s.Svc.Status(sctx, p)
		cancel()
		switch {
		case err == nil && st.Media != nil && st.Media.ID != "unknown":
			d.MediaReported = true
		case err == nil || errors.Is(err, driver.ErrNoReadback):
			d.AskMedia = true
			return
		default:
			d.Msg = "The printer isn't ready: " + err.Error()
			return
		}
	}
	// a label already being sent finishes even if the window closes or the browser goes away
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	s.setupPrints.Add(1) // serve waits for it before exiting
	defer s.setupPrints.Done()
	res, err := s.Svc.Print(ctx, p, "", printing.TestCard(p.Model, s.Version))
	s.finish("setup wizard", "test label", p.ID, res, err)
	if err != nil {
		d.Msg = "The test label didn't print: " + err.Error()
		return
	}
	d.Printed = true
}

// statusLine is the printer's status in words, for the faint/nothing pictures.
func (s *Server) statusLine(ctx context.Context, d *Draft) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := s.Svc.Status(ctx, &d.Printer)
	switch {
	case errors.Is(err, driver.ErrNoReadback):
		return "The printer is reachable but doesn't report its state."
	case err != nil:
		return "The printer says: " + err.Error() + "."
	}
	text, _ := statusText(s.Svc, st, &d.Printer)
	return "The printer says: " + text + "."
}

// --- step 4: name

func nameIdeas(d *Draft) []string {
	return []string{"Receiving desk", "Workbench", "Stores", d.Printer.Model}
}

func (s *Server) setupName(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	if !d.Printed {
		http.Redirect(w, r, "/setup/test", http.StatusSeeOther)
		return
	}
	s.setupPage(w, r, "setup-name", 4, map[string]any{"Ideas": nameIdeas(d)})
}

func (s *Server) setupNamePost(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	if d.SavedCurrent { // a repeated Save
		http.Redirect(w, r, "/setup/done", http.StatusSeeOther)
		return
	}
	if !d.Printed {
		http.Redirect(w, r, "/setup/test", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 60 {
		s.setupPage(w, r, "setup-name", 4, map[string]any{"Err": "Give the printer a name (up to 60 characters).", "Ideas": nameIdeas(d)})
		return
	}
	p := d.Printer
	p.Name = name
	var instID string
	err := s.updateUndo(func(c *config.Config) (undo func(), err error) {
		if d.Instance != nil {
			instID, _, undo = s.putInstance(c, d.Instance.BaseURL, d.Instance.Token, d.Instance.AllowHTTP)
		}
		p.ID = uniqueID(c, slug(name))
		c.Printers = append(c.Printers, p)
		return undo, nil
	})
	if err != nil {
		s.setupPage(w, r, "setup-name", 4, map[string]any{"Err": "Couldn't save: " + err.Error(), "Ideas": nameIdeas(d)})
		return
	}
	if d.Instance != nil {
		d.Instance = nil
		s.instanceSaved(instID)
	}
	d.Saved, d.SavedCurrent = append(d.Saved, name), true
	http.Redirect(w, r, "/setup/done", http.StatusSeeOther)
}

// slug makes a printer id from a name: "Receiving desk" → "receiving-desk".
func slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return cmp.Or(b.String(), "printer")
}

// uniqueID returns id, or id-2, id-3 … when a printer already has it.
func uniqueID(c *config.Config, id string) string {
	taken := func(id string) bool {
		return slices.ContainsFunc(c.Printers, func(p config.Printer) bool { return p.ID == id })
	}
	out := id
	for n := 2; taken(out); n++ {
		out = id + "-" + strconv.Itoa(n)
	}
	return out
}

// --- step 5: done

func (s *Server) setupDone(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	if len(d.Saved) == 0 {
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	cfg := s.config()
	s.setupPage(w, r, "setup-done", 5, map[string]any{"TelemetryOn": cfg.Telemetry == nil || *cfg.Telemetry})
}

func (s *Server) setupDonePost(w http.ResponseWriter, r *http.Request) {
	sess, d, unlock := lockDraft(r)
	defer unlock()
	on := r.FormValue("telemetry") == "1" // passing this step records the choice (docs/telemetry.md)
	if err := s.update(func(c *config.Config) error { c.Telemetry = &on; return nil }); err != nil {
		s.Log.Warn("setup: couldn't save the usage report choice", "err", err)
	}
	if r.FormValue("action") == "another" {
		d.resetPrinter(config.Printer{})
		http.Redirect(w, r, "/setup/printer", http.StatusSeeOther)
		return
	}
	if sess == s.local {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.setupPage(w, r, "setup-finished", 5, nil)
	// a moment later, so the page can still load its stylesheet; a window reopened meanwhile stays
	time.AfterFunc(s.finishGrace, func() { s.Window.CloseGeneration(sess.Gen) })
}

// --- report

func (s *Server) setupReport(w http.ResponseWriter, r *http.Request) {
	s.setupPage(w, r, "setup-report", 0, map[string]any{"IssueURL": issueURL, "Candidate": r.URL.Query().Get("candidate")})
}

func (s *Server) setupReportPost(w http.ResponseWriter, r *http.Request) {
	_, d, unlock := lockDraft(r)
	defer unlock()
	var targets []string
	target := func(c discovery.Candidate) {
		if c.Kind == "usb" {
			targets = []string{"usb"}
		} else {
			targets = []string{c.Host}
		}
	}
	if i, err := strconv.Atoi(r.FormValue("candidate")); err == nil && i >= 0 && i < len(d.Candidates) {
		target(d.Candidates[i])
	} else if d.Printer.Model != "" {
		t := d.Printer.Transport
		target(discovery.Candidate{Kind: t.Kind, Host: t.Host})
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	rep, err := report.Run(ctx, report.Options{Version: s.Version, DB: devicedb.Default(), Targets: targets, Out: io.Discard, Svc: s.Svc})
	if err != nil {
		http.Error(w, "couldn't make the report: "+err.Error(), http.StatusInternalServerError)
		return
	}
	b, err := rep.JSON(false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pdb-print-report-%s.json"`, time.Now().Format("20060102-150405")))
	_, _ = w.Write(b)
}

// WaitSetupPrints waits for wizard test prints still running (they outlive their request).
func (s *Server) WaitSetupPrints() { s.setupPrints.Wait() }
