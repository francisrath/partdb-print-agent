// Package server is the agent's local HTTP API and web UI (architecture §4.1, §10).
// It binds to loopback only, rejects foreign Host headers (DNS rebinding) and
// requires a CSRF token on every POST.
package server

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/francisrath/partdb-print-agent/assets"
	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/zalando/go-keyring"
)

//go:embed web
var webFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"levelName": func(l int) string {
		switch l {
		case 1:
			return "Read-Only ✓"
		case 2:
			return "Edit (more than needed; Read-Only is enough)"
		case 3, 4:
			return "Admin/Full (more than needed; Read-Only is enough)"
		}
		return "unknown level"
	},
	"steps": func() []string { return []string{"Part-DB", "Printer", "Test", "Name", "Done"} },
	"add":   func(a, b int) int { return a + b },
	"sub":   func(a, b int) int { return a - b },
}).ParseFS(webFS, "web/*.html", "web/setup/*.html"))

const handoffWindow = 120 * time.Second

type Server struct {
	CfgPath string
	Svc     *printing.Service
	Log     *slog.Logger
	Addr    string // host:port the listener is bound to (loopback)
	Version string
	// OnInstance is called after a Part-DB instance with a token was saved (token filled in),
	// so its print jobs are fetched without a restart. nil = nothing.
	OnInstance func(in config.Instance)
	// Window is the setup window (see window.go); LANAddrs overrides LocalAddrs in tests.
	Window      *Window
	LANAddrs    func() []netip.Addr
	LANNets     func() []netip.Prefix // overrides LocalNets in tests
	kick        chan struct{}         // wakes SetupLoop
	scanned     chan struct{}         // SetupLoop finished a scan
	boundMu     sync.Mutex
	bound       []netip.Addr   // LAN addresses the setup server listens on
	loopRunning atomic.Bool    // SetupLoop is running (OpenWindow waits for its scan)
	local       *Session       // the wizard's session on loopback (no code needed there)
	finishGrace time.Duration  // after Finish the window stays open this long (the page\'s stylesheet)
	setupPrints sync.WaitGroup // wizard test prints in progress
	// Discover finds printers for the wizard (ip "" = USB, the LAN and Bluetooth); nil = discovery package.
	Discover func(ctx context.Context, ip string) ([]discovery.Candidate, error)

	mu    sync.Mutex // guards cfg, flash, jobs, seen
	cfg   *config.Config
	csrf  string
	flash *flash
	jobs  []Job
	seen  map[string]time.Time // used handoff signatures
}

type flash struct {
	Text string
	Err  bool
}

// Job is one entry of the in-memory history.
// ponytail: in-memory, last 50; a durable queue (bbolt) comes with retries.
type Job struct {
	Time            time.Time
	Source, What    string
	Printer, Result string
	Err             string
}

func New(cfg *config.Config, cfgPath string, svc *printing.Service, log *slog.Logger, addr, version string) *Server {
	s := &Server{cfg: cfg, CfgPath: cfgPath, Svc: svc, Log: log, Addr: addr, Version: version,
		csrf: randomHex(32), seen: map[string]time.Time{}, Window: &Window{}, kick: make(chan struct{}, 1), scanned: make(chan struct{}, 1)}
	s.Window.OnClose = s.kickSetup
	s.local = &Session{ID: "local", CSRF: s.csrf, Draft: &Draft{}}
	s.finishGrace = 10 * time.Second
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"name": "pdb-print", "version": s.Version})
	})
	mux.HandleFunc("POST /instance", s.saveInstance)
	mux.HandleFunc("POST /instance/rotate-key", s.rotateKey)
	mux.HandleFunc("POST /printers", s.savePrinter)
	mux.HandleFunc("POST /printers/{id}/delete", s.deletePrinter)
	mux.HandleFunc("POST /printers/{id}/test", s.testPrint)
	mux.HandleFunc("GET /printers/{id}/status", s.printerStatus)
	mux.HandleFunc("POST /routes", s.saveRoute)
	mux.HandleFunc("POST /routes/{profile}/delete", s.deleteRoute)
	mux.HandleFunc("POST /v1/pair", s.pairAPI)
	mux.HandleFunc("GET /setup/{$}", func(w http.ResponseWriter, r *http.Request) {
		d := s.local.Draft // a fresh start
		d.mu.Lock()
		d.Instance, d.Notes, d.NoAgentAPI, d.APIChecked, d.APIErr, d.Candidates, d.Scanned, d.Saved = nil, nil, false, false, "", nil, false, nil
		d.resetPrinter(config.Printer{})
		d.mu.Unlock()
		http.Redirect(w, r, "/setup/partdb", http.StatusSeeOther)
	})
	s.setupRoutes(mux)
	mux.HandleFunc("GET /v1/handoff", s.handoff)
	mux.HandleFunc("POST /v1/handoff", s.handoff)

	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	icons, _ := fs.Sub(assets.Web, "icon/web")
	for _, f := range []string{"favicon.svg", "favicon.ico", "apple-touch-icon.png"} {
		mux.Handle("GET /"+f, http.FileServerFS(icons))
	}
	return s.guard(s.withLocalSession(mux))
}

// guard enforces the loopback Host allowlist, CSRF on POST and safe headers.
func (s *Server) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.Addr)
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true,
		s.Addr: true} // e.g. 127.0.0.2:8631; serve only binds loopback addresses
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		securityHeaders(w)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/pair" {
			// `pdb-print pair` on this machine: no CSRF token, but never from a browser or the LAN
			if r.Header.Get("Origin") != "" || !loopbackRemote(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			origin := r.Header.Get("Origin")
			originOK := origin == "" || origin == "http://"+r.Host
			tokenOK := subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.csrf)) == 1
			if !originOK || !tokenOK {
				s.Log.Warn("rejected POST", "path", r.URL.Path, "origin", origin, "origin_ok", originOK, "csrf_ok", tokenOK)
				http.Error(w, "invalid or missing CSRF token: reload the page", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin") // "no-referrer" makes browsers send Origin: null on POST
	h.Set("Cache-Control", "no-store")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, text string, err error) {
	s.mu.Lock()
	if err != nil {
		s.flash = &flash{Text: text + ": " + err.Error(), Err: true}
	} else {
		s.flash = &flash{Text: text}
	}
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// update applies fn to a copy of the config, validates and saves it, then swaps it in.
func (s *Server) update(fn func(c *config.Config) error) error {
	return s.updateUndo(func(c *config.Config) (func(), error) { return nil, fn(c) })
}

// updateUndo is update for changes outside the config (the keychain): when the config isn't
// saved, the undo fn returns runs before the lock is released, so no later save is overwritten.
func (s *Server) updateUndo(fn func(c *config.Config) (undo func(), err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.cfg.Clone()
	if err != nil {
		return err
	}
	undo, err := fn(c)
	if err == nil {
		err = c.Validate()
	}
	if err == nil {
		err = c.Save(s.CfgPath)
	}
	if err != nil {
		if undo != nil {
			undo()
		}
		return err
	}
	s.cfg = c
	return nil
}

func (s *Server) config() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Server) record(j Job) {
	j.Time = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append([]Job{j}, s.jobs...)
	if len(s.jobs) > 50 {
		s.jobs = s.jobs[:50]
	}
}

type routeView struct {
	config.Route
	Bookmarklet template.URL
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	data := map[string]any{
		"Version": s.Version, "Addr": s.Addr, "CSRF": s.csrf, "FromEnv": false,
		"Printers": cfg.Printers, "Models": formModels(),
	}
	data["Media"] = formMedia()

	var in *config.Instance
	if len(cfg.Instances) > 0 {
		in = &cfg.Instances[0]
	} else if env, err := cfg.Instance(""); err == nil {
		// connected through PDB_PRINT_URL/PDB_PRINT_TOKEN; saving the form makes it permanent
		in = &config.Instance{ID: env.ID, BaseURL: env.BaseURL}
		data["FromEnv"] = true
	}
	if in != nil {
		data["Instance"] = in
		if in.PairingKey != "" {
			// Part-DB's print button setup (docs/usage/labels.md there); the fragment never reaches Part-DB's server
			data["ConnectURL"] = strings.TrimSuffix(in.BaseURL, "/") + "/label/agent#url=" + url.QueryEscape("http://"+s.Addr) + "&key=" + in.PairingKey
		}
		if full, err := cfg.Instance(in.ID); err != nil {
			data["TokenErr"] = err.Error()
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			c := partdb.FromInstance(full)
			if tok, err := c.TokenInfo(ctx); err != nil {
				data["TokenErr"] = err.Error()
			} else {
				data["Token"] = tok
			}
			if ps, err := c.Profiles(ctx); err != nil {
				data["ProfilesErr"] = err.Error()
			} else {
				data["Profiles"] = ps
			}
			cancel()
		}
	}
	var routes []routeView
	for _, rt := range cfg.Routes {
		v := routeView{Route: rt}
		if in != nil && in.PairingKey != "" {
			v.Bookmarklet = template.URL(bookmarklet(in.PairingKey, rt.Profile, "http://"+s.Addr))
		}
		routes = append(routes, v)
	}
	data["Routes"] = routes
	if hf := cfg.HotFolder; hf.Enabled {
		dir, _ := hf.Path()
		data["HotFolder"] = fmt.Sprintf("%s (%s) → %s", dir, defaultStr(hf.Pattern, "label_*.pdf"), hf.Printer)
	}

	s.mu.Lock()
	data["Flash"], s.flash = s.flash, nil
	data["Jobs"] = append([]Job(nil), s.jobs...)
	s.mu.Unlock()
	if err := pages.ExecuteTemplate(w, "index.html", data); err != nil {
		s.Log.Error("render page", "err", err)
	}
}

func (s *Server) saveInstance(w http.ResponseWriter, r *http.Request) {
	baseURL := strings.TrimRight(strings.TrimSpace(r.FormValue("base_url")), "/")
	token := strings.TrimSpace(r.FormValue("token"))
	var warn, id string
	err := s.updateUndo(func(c *config.Config) (undo func(), err error) {
		if len(c.Instances) == 0 {
			c.Instances = append(c.Instances, config.Instance{ID: "default"})
		}
		in := &c.Instances[0]
		if token == "" {
			cur, err := c.Instance(in.ID)
			if err != nil {
				return nil, errors.New("enter an API token")
			}
			token = cur.Token
		}
		in.BaseURL = baseURL
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		probe := *in
		probe.Token = token
		tok, err := partdb.FromInstance(&probe).TokenInfo(ctx)
		if err != nil {
			return nil, err
		}
		if tok.Level > 1 {
			warn = " (this token can do more than needed: a Read-Only token is enough)"
		}
		var note string
		id, note, undo = s.putInstance(c, baseURL, token, in.AllowHTTP)
		warn += note
		return undo, nil
	})
	if err == nil {
		s.instanceSaved(id)
	}
	s.redirect(w, r, "Connected to Part-DB"+warn, err)
}

// putInstance stores the first Part-DB instance's URL and token (in the OS keychain when there
// is one, else in the 0600 config file); it returns the instance id, a note for the user (or "")
// and, when it changed the keychain, a func that undoes that if the config isn't saved.
func (s *Server) putInstance(c *config.Config, baseURL, token string, allowHTTP bool) (id, note string, undo func()) {
	if len(c.Instances) == 0 {
		c.Instances = append(c.Instances, config.Instance{ID: "default"})
	}
	in := &c.Instances[0]
	in.BaseURL, in.AllowHTTP = baseURL, allowHTTP
	if in.PairingKey == "" {
		in.PairingKey = randomHex(32)
	}
	prev, prevErr := keyring.Get(config.KeyringService, in.ID)
	if err := keyring.Set(config.KeyringService, in.ID, token); err != nil {
		// normal on a headless server (no desktop keychain), so info, not a warning
		s.Log.Info("no OS keychain; the token is kept in the config file, readable only by this user", "err", err)
		in.Token = token
		return in.ID, " (token kept in the config file, readable only by pdb-print)", nil
	}
	in.Token = ""
	id = in.ID
	// if the config can't be saved, the keychain must keep the token that goes with the old URL
	return id, "", func() {
		if prevErr == nil {
			_ = keyring.Set(config.KeyringService, id, prev)
		} else {
			_ = keyring.Delete(config.KeyringService, id)
		}
	}
}

// Instance is a Part-DB instance as currently saved (token filled in).
func (s *Server) Instance(id string) (*config.Instance, error) { return s.config().Instance(id) }

// instanceSaved hands a newly saved instance (with its token) to OnInstance.
func (s *Server) instanceSaved(id string) {
	if s.OnInstance == nil {
		return
	}
	in, err := s.config().Instance(id)
	if err != nil {
		s.Log.Warn("not fetching print jobs", "instance", id, "reason", err)
		return
	}
	s.OnInstance(*in)
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	err := s.update(func(c *config.Config) error {
		if len(c.Instances) == 0 {
			return errors.New("no Part-DB instance")
		}
		c.Instances[0].PairingKey = randomHex(32)
		return nil
	})
	s.redirect(w, r, "New key: drag the bookmarklets to your bookmarks bar again", err)
}

func (s *Server) savePrinter(w http.ResponseWriter, r *http.Request) {
	port, _ := strconv.Atoi(r.FormValue("port"))
	p := config.Printer{
		ID: strings.TrimSpace(r.FormValue("id")), Name: strings.TrimSpace(r.FormValue("name")),
		Model:     r.FormValue("model"),
		Transport: config.Transport{Kind: "tcp", Host: strings.TrimSpace(r.FormValue("host")), Port: port},
		Media:     r.FormValue("media"), Rotate: r.FormValue("rotate"), Cut: r.FormValue("cut"),
	}
	err := s.update(func(c *config.Config) error {
		for _, old := range c.Printers {
			if old.ID != p.ID {
				continue
			}
			// the form only knows network QLs
			if m, err := old.DeviceModel(); err != nil || !slices.Contains(formModels(), m.ID) || old.Transport.Kind != "tcp" {
				return fmt.Errorf("printer %q (%s over %s): edit it in the config file", p.ID, old.Model, old.Transport.Kind)
			}
		}
		if !slices.Contains(formModels(), p.Model) {
			return fmt.Errorf("unknown Brother QL model %s", p.Model)
		}
		if f := p.FallbackMedia(); f != "" {
			if _, ok := printing.LookupMedia(&p, f); !ok {
				return fmt.Errorf("unknown media %q", f)
			}
		}
		for i := range c.Printers {
			if c.Printers[i].ID == p.ID {
				// keep settings the form doesn't show
				old := c.Printers[i]
				p.Threshold, p.SNMPCommunity, p.NoSNMP, p.AllowScaling = old.Threshold, old.SNMPCommunity, old.NoSNMP, old.AllowScaling
				p.Mirror, p.UpsideDown, p.MediaAllowed, p.BlackMarkMM = old.Mirror, old.UpsideDown, old.MediaAllowed, old.BlackMarkMM
				c.Printers[i] = p
				return nil
			}
		}
		c.Printers = append(c.Printers, p)
		return nil
	})
	s.redirect(w, r, "Saved printer "+p.ID, err)
}

func (s *Server) deletePrinter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.update(func(c *config.Config) error {
		var ps []config.Printer
		for _, p := range c.Printers {
			if p.ID != id {
				ps = append(ps, p)
			}
		}
		var rs []config.Route
		for _, rt := range c.Routes {
			if rt.Printer != id {
				rs = append(rs, rt)
			}
		}
		c.Printers, c.Routes = ps, rs
		return nil
	})
	s.redirect(w, r, "Removed printer "+id+" and its routes", err)
}

func (s *Server) saveRoute(w http.ResponseWriter, r *http.Request) {
	profile, _ := strconv.Atoi(r.FormValue("profile"))
	printer := r.FormValue("printer")
	name := fmt.Sprintf("profile %d", profile)
	if full, err := s.config().Instance(""); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if ps, err := partdb.FromInstance(full).Profiles(ctx); err == nil {
			for _, p := range ps {
				if p.ID == profile {
					name = p.Name
				}
			}
		}
		cancel()
	}
	err := s.update(func(c *config.Config) error {
		rt := config.Route{Profile: profile, Name: name, Printer: printer}
		for i := range c.Routes {
			if c.Routes[i].Profile == profile && c.Routes[i].Instance == "" { // UI routes apply to all instances
				c.Routes[i] = rt
				return nil
			}
		}
		c.Routes = append(c.Routes, rt)
		return nil
	})
	s.redirect(w, r, "Profile "+name+" → "+printer, err)
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	profile, _ := strconv.Atoi(r.PathValue("profile"))
	instance := r.FormValue("instance")
	err := s.update(func(c *config.Config) error {
		var rs []config.Route
		for _, rt := range c.Routes {
			if rt.Profile != profile || rt.Instance != instance {
				rs = append(rs, rt)
			}
		}
		c.Routes = rs
		return nil
	})
	s.redirect(w, r, "Removed route", err)
}

func (s *Server) printer(id string) (*config.Printer, error) {
	p, err := s.config().Printer(id)
	if err != nil {
		return nil, err
	}
	cp := *p
	return &cp, nil
}

func (s *Server) testPrint(w http.ResponseWriter, r *http.Request) {
	p, err := s.printer(r.PathValue("id"))
	if err != nil {
		s.redirect(w, r, "Test print failed", err)
		return
	}
	res, err := s.Svc.Print(r.Context(), p, "", printing.TestCard(p.Model, s.Version))
	s.record(jobFor("web UI", "test label", p.ID, res, err))
	s.redirect(w, r, "Test label "+resultText(res), err)
}

func (s *Server) printerStatus(w http.ResponseWriter, r *http.Request) {
	p, err := s.printer(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := s.Svc.Status(ctx, p)
	switch {
	case errors.Is(err, driver.ErrNoReadback) && needsStatus(p):
		writeJSON(w, map[string]any{"ok": false, "text": "no status reply: " + checkHint(p.Transport.Kind)})
	case errors.Is(err, driver.ErrNoReadback) && p.FallbackMedia() == "":
		writeJSON(w, map[string]any{"ok": false, "text": "reachable, but it reports no status or roll: set media for this printer"})
	case errors.Is(err, driver.ErrNoReadback):
		writeJSON(w, map[string]any{"ok": true, "text": "reachable (no status over TCP)"})
	case err != nil:
		writeJSON(w, map[string]any{"ok": false, "text": err.Error()})
	default:
		text, ok := statusText(s.Svc, st, p)
		if m, _, _ := s.Svc.MediaStatus(p, st); m != nil {
			pctx, cancel := context.WithTimeout(r.Context(), 20*time.Second) // first call measures each profile
			defer cancel()
			if fit := s.Svc.FittingProfiles(pctx, s.config(), m); fit != "" {
				text += " · " + fit
			}
		}
		writeJSON(w, map[string]any{"ok": ok, "text": text})
	}
}

// statusText summarises a printer status for the UI; ok=false also when jobs would be
// refused or fail for the loaded roll.
func statusText(svc *printing.Service, st *driver.Status, p *config.Printer) (string, bool) {
	_, media, ok := svc.MediaStatus(p, st)
	text := fmt.Sprintf("%s · %s · media: %s", st.Code, cmp.Or(st.Model, p.Model), media)
	if st.Detail != "" {
		text += " · " + st.Detail
	}
	return text, ok && st.Code == driver.OK
}

// PrintFile prints a PDF file from another job source (e.g. the hot folder) on printerID,
// using the current config, and records it in the job history.
func (s *Server) PrintFile(ctx context.Context, source, printerID, path string) error {
	p, err := s.printer(printerID)
	if err != nil {
		return err
	}
	pdf, err := render.ReadFile(path)
	var res *printing.Result
	if err == nil {
		res, err = s.Svc.Print(ctx, p, "", printing.PDF(pdf))
	}
	s.record(jobFor(source, filepath.Base(path), p.ID, res, err))
	return err
}

// EnsureAgentID returns the agent id for Part-DB's print job API, generating and saving it on first use.
func (s *Server) EnsureAgentID() (string, error) {
	if id := s.config().AgentID; id != "" {
		return id, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80 // UUID version 4, RFC 4122 variant
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	return id, s.update(func(c *config.Config) error {
		c.AgentID = id
		return nil
	})
}

// AgentPrinters are the configured printers, as reported to Part-DB.
func (s *Server) AgentPrinters() []partdb.AgentPrinter {
	ps := s.config().Printers
	out := make([]partdb.AgentPrinter, len(ps))
	var wg sync.WaitGroup
	for i := range ps {
		p := &ps[i]
		out[i] = partdb.AgentPrinter{ID: p.ID, Name: p.Name, Model: p.Model, Media: p.FallbackMedia()}
		if m, _, ok := s.Svc.LastMedia(p.ID); ok {
			out[i].Media = m.ID // the roll it reported last
		}
		wg.Add(1)
		go func() { defer wg.Done(); out[i].Online = s.Svc.Online(p) }() // checks take up to 2 s each
	}
	wg.Wait()
	return out
}

// PrintJob prints a print job from Part-DB's job API on the printer the job names.
func (s *Server) PrintJob(ctx context.Context, instanceID string, job partdb.Job) (string, error) {
	pl := job.Payload
	what := fmt.Sprintf("%s %s · profile %d", pl.ElementType, pl.ElementIDs, pl.ProfileID)
	p, err := s.printer(job.Printer)
	if err != nil {
		s.record(jobFor("Part-DB job", what, job.Printer, nil, err))
		return "", err
	}
	var res *printing.Result
	in, err := s.config().Instance(instanceID)
	if err == nil {
		var pdf []byte
		if pdf, err = partdb.FromInstance(in).GenerateLabels(ctx, pl.ProfileID, pl.ElementIDs, pl.ElementType); err == nil {
			res, err = s.Svc.Print(ctx, p, "", printing.PDF(pdf))
		}
	}
	s.record(jobFor("Part-DB job", what, p.ID, res, err))
	if err != nil {
		return "", err
	}
	return resultText(res), nil
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func jobFor(source, what, printer string, res *printing.Result, err error) Job {
	j := Job{Source: source, What: what, Printer: printer}
	if err != nil {
		j.Err = err.Error()
	} else {
		j.Result = resultText(res)
	}
	return j
}

func resultText(res *printing.Result) string {
	if res == nil {
		return "not printed"
	}
	s := fmt.Sprintf("%d page(s) on %s", res.Pages, res.Media)
	if res.MediaFrom != "" {
		s += " (" + res.MediaFrom + ")"
	}
	if !res.Confirmed {
		s += ", sent (unconfirmed)"
	}
	if len(res.Warnings) > 0 {
		s += "; warning: " + strings.Join(res.Warnings, "; ")
	}
	return s
}

// --- bookmarklet handoff ---

var idsRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)

// canonical is the signed message; the bookmarklet builds the same string.
func canonical(src, typ, ids, profile, ts string) string {
	return strings.Join([]string{src, typ, ids, profile, ts}, "\n")
}

func sign(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// bookmarklet opens the handoff window first (keeping the click's user activation),
// signs the request with WebCrypto, then navigates the window to the agent. Without
// WebCrypto (Part-DB over plain http) it sends the request unsigned, and the agent
// asks for an explicit click instead of printing straight away.
func bookmarklet(key string, profile int, agent string) string {
	js := `(async()=>{const m=location.pathname.match(/\/(part|store_location)\/(\d+)/);` +
		`if(!m){alert('pdb-print: open a part or storage location page first');return}` +
		`const w=window.open('about:blank','pdbprint','popup,width=420,height=340');` +
		`const p={src:location.origin,type:m[1]==='part'?'part':'storelocation',ids:m[2],profile:'` + strconv.Itoa(profile) + `',ts:String(Math.floor(Date.now()/1000))};` +
		`const go=()=>{(w||window).location.href='` + agent + `/v1/handoff?'+new URLSearchParams(p)};` +
		// no WebCrypto on plain-http pages: send unsigned; the agent then asks for a click
		`if(!(window.crypto&&crypto.subtle)){go();return}` +
		`const e=new TextEncoder(),k=await crypto.subtle.importKey('raw',e.encode('` + key + `'),{name:'HMAC',hash:'SHA-256'},false,['sign']);` +
		`const s=await crypto.subtle.sign('HMAC',k,e.encode([p.src,p.type,p.ids,p.profile,p.ts].join('\n')));` +
		`p.sig=Array.from(new Uint8Array(s),b=>b.toString(16).padStart(2,'0')).join('');go()})()`
	return "javascript:" + url.PathEscape(js)
}

type handoffView struct {
	What, Printer, Result, Why, Err, CSRF string
	Done, AutoClose                       bool
	Params                                map[string]string
}

func (s *Server) handoff(w http.ResponseWriter, r *http.Request) {
	v := handoffView{CSRF: s.csrf}
	defer func() {
		if err := pages.ExecuteTemplate(w, "handoff.html", v); err != nil {
			s.Log.Error("render handoff", "err", err)
		}
	}()
	src, typ, ids, profileS, ts, sig := r.FormValue("src"), r.FormValue("type"), r.FormValue("ids"), r.FormValue("profile"), r.FormValue("ts"), r.FormValue("sig")
	v.Params = map[string]string{"src": src, "type": typ, "ids": ids, "profile": profileS}

	cfg := s.config()
	var in *config.Instance
	for i := range cfg.Instances {
		if sameOrigin(cfg.Instances[i].BaseURL, src) {
			in = &cfg.Instances[i]
		}
	}
	profile, perr := strconv.Atoi(profileS)
	switch {
	case in == nil:
		v.Err = fmt.Sprintf("%q is not a configured Part-DB instance.", src)
		return
	case typ != "part" && typ != "part_lot" && typ != "storelocation":
		v.Err = "Unsupported element type."
		return
	case len(ids) > 8192: // Part-DB's bulk print lists every id
		v.Err = "Too many labels for one request: print them in smaller batches."
		return
	case !idsRe.MatchString(ids):
		v.Err = "Invalid element ids."
		return
	case perr != nil:
		v.Err = "Invalid profile."
		return
	}
	rt, ok := cfg.Route(in.ID, profile)
	if !ok {
		v.Err = fmt.Sprintf("No printer is set up for label profile %d. Add a route in the pdb-print UI.", profile)
		return
	}
	v.What = fmt.Sprintf("%s %s · %s", typ, ids, rt.Name)
	v.Printer = rt.Printer

	if r.Method == http.MethodGet {
		if why := s.checkSignature(in.PairingKey, canonical(src, typ, ids, profileS, ts), ts, sig); why != "" {
			v.Why = why // ask for an explicit click instead
			return
		}
	}
	p, err := s.printer(rt.Printer)
	if err == nil {
		var full *config.Instance
		if full, err = cfg.Instance(in.ID); err == nil {
			var pdf []byte
			if pdf, err = partdb.FromInstance(full).GenerateLabels(r.Context(), profile, ids, typ); err == nil {
				var res *printing.Result
				res, err = s.Svc.Print(r.Context(), p, "", printing.PDF(pdf))
				v.Result = resultText(res)
				s.record(jobFor("browser", v.What, p.ID, res, err))
			}
		}
	}
	if err != nil {
		v.Err = err.Error()
		return
	}
	v.Done, v.AutoClose = true, true
}

// checkSignature returns why a GET handoff may not print without a click ("" = ok).
func (s *Server) checkSignature(key, msg, ts, sig string) string {
	if sig == "" || key == "" {
		return "This request is not signed by your bookmarklet or Part-DB print button."
	}
	if !hmac.Equal([]byte(sign(key, msg)), []byte(strings.ToLower(sig))) {
		return "The signature doesn't match: after a key rotation, drag the bookmarklets again or reconnect Part-DB's print button."
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(t, 0)).Abs() > handoffWindow {
		return "This link has expired."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, at := range s.seen {
		if time.Since(at) > 2*handoffWindow {
			delete(s.seen, k)
		}
	}
	if _, dup := s.seen[sig]; dup {
		return "This label was already printed from this link."
	}
	s.seen[sig] = time.Now()
	return ""
}

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Scheme != "" && ua.Scheme == ub.Scheme && strings.EqualFold(ua.Host, ub.Host)
}

// formModels are the models the web form can set up: network Brother QLs. Other printers
// are configured in the config file until the setup wizard.
func formModels() []string {
	var out []string
	for _, m := range devicedb.Default().Models("brother") {
		if slices.Contains(m.Transports, "tcp") {
			out = append(out, m.ID)
		}
	}
	return out
}

// checkHint says what to check on a printer that doesn't answer.
func checkHint(kind string) string {
	switch kind {
	case "ble":
		return "check that it's switched on, in range and not connected to a phone"
	case "bt":
		return "check that it's switched on, paired with this computer and in range"
	case "usb":
		return "check the printer's power and USB cable"
	}
	return "check the printer's power and network connection"
}

// needsStatus: printers that can't print without a status reply (PT) are not OK when silent.
func needsStatus(p *config.Printer) bool {
	m, err := p.DeviceModel()
	return err == nil && m.QuirkBool("needs_status")
}

type formMediaOption struct {
	devicedb.Media
	Models string // space-separated model ids that take it (the form shows those for the selected model)
}

// formMedia lists the media of all form models, each tagged with the models that take it.
func formMedia() []formMediaOption {
	db := devicedb.Default()
	var out []formMediaOption
	idx := map[string]int{}
	for _, id := range formModels() {
		m, _ := db.Model(id)
		for _, md := range db.MediaFor(m) {
			if i, ok := idx[md.ID]; ok {
				out[i].Models += " " + id
				continue
			}
			idx[md.ID] = len(out)
			out = append(out, formMediaOption{md, id})
		}
	}
	return out
}
