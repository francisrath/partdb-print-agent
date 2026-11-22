package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/francisrath/pdb-print/assets"
)

// The setup window: for a while after install (or `pdb-print pair`) the wizard is reachable from
// other computers on the LAN, after typing a 6-digit code shown where pdb-print runs.
const (
	windowOpen  = 15 * time.Minute // without use
	windowIdle  = 5 * time.Minute  // left at least this long after each wizard request
	windowMax   = 30 * time.Minute // never longer than this after opening
	windowTries = 5                // wrong codes before it closes
	cookieName  = "pdb_setup"
)

// Window is the setup window's state. The zero value is closed.
type Window struct {
	mu       sync.Mutex
	open     bool
	code     string
	used     bool // the code paired a browser: it is single-use
	tries    int
	opened   time.Time
	expires  time.Time
	gen      uint64 // counts Opens (two can share a clock tick)
	sessions map[string]*Session
	// OnClose is called (without the lock) whenever the window closes.
	OnClose func()
}

// Session is one browser that typed the code.
type Session struct {
	ID, CSRF string
	Draft    *Draft
	Gen      uint64 // the window it was paired in
}

// Open starts a new window with a new code; earlier sessions end.
func (w *Window) Open(now time.Time) (code string, expires time.Time) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.open, w.code, w.used, w.tries, w.gen = true, fmt.Sprintf("%06d", n.Int64()), false, 0, w.gen+1
	w.opened, w.expires = now, now.Add(windowOpen)
	w.sessions = map[string]*Session{}
	return w.code, w.expires
}

// IsOpen reports whether the window is open at now, closing it when it has expired.
func (w *Window) IsOpen(now time.Time) bool {
	w.mu.Lock()
	open := w.open && now.Before(w.expires.Add(time.Nanosecond))
	closed := w.open && !open && w.closeLocked()
	w.mu.Unlock()
	if closed {
		w.notifyClose()
	}
	return open
}

// Check tries code; a right one starts a session and uses the code up (it pairs one browser), a
// wrong one counts towards closing the window. Once the code is used, nothing pairs and tries no
// longer count: there is nothing left to guess, and closing would end the paired browser's session.
func (w *Window) Check(code string, now time.Time) (*Session, bool) {
	if !w.IsOpen(now) {
		return nil, false
	}
	w.mu.Lock()
	if !w.open || w.code == "" || w.used { // closed meanwhile, or the code is used up
		w.mu.Unlock()
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(w.code)) != 1 {
		w.tries++
		closed := w.tries >= windowTries && w.closeLocked() // under the lock: no session after the 5th
		w.mu.Unlock()
		if closed {
			w.notifyClose()
		}
		return nil, false
	}
	defer w.mu.Unlock()
	s := &Session{ID: randomHex(32), CSRF: randomHex(32), Draft: &Draft{}, Gen: w.gen}
	w.sessions[s.ID] = s
	w.used = true
	return s, true
}

// CodeUsed reports whether this window's code already paired a browser.
func (w *Window) CodeUsed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.used
}

// Session returns the request's session while the window is open.
func (w *Window) Session(r *http.Request, now time.Time) (*Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || !w.IsOpen(now) {
		return nil, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.sessions[c.Value]
	return s, ok
}

// Touch keeps the window open at least windowIdle longer, up to windowMax after it opened.
func (w *Window) Touch(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.open {
		if e := now.Add(windowIdle); e.After(w.expires) {
			w.expires = e
		}
		if limit := w.opened.Add(windowMax); w.expires.After(limit) {
			w.expires = limit
		}
	}
}

// Generation identifies the current window; each Open starts a new one.
func (w *Window) Generation() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gen
}

// CloseGeneration closes the window only if it is still generation g.
func (w *Window) CloseGeneration(g uint64) {
	w.mu.Lock()
	closed := w.gen == g && w.closeLocked()
	w.mu.Unlock()
	if closed {
		w.notifyClose()
	}
}

// Expires is when the window closes unless it is used.
func (w *Window) Expires() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expires
}

// Close ends the window and every session.
func (w *Window) Close() {
	w.mu.Lock()
	closed := w.closeLocked()
	w.mu.Unlock()
	if closed {
		w.notifyClose()
	}
}

// closeLocked closes the window (w.mu held) and reports whether it was open.
func (w *Window) closeLocked() bool {
	was := w.open
	w.open, w.code, w.sessions = false, "", nil
	return was
}

func (w *Window) notifyClose() {
	if w.OnClose != nil {
		w.OnClose()
	}
}

// OpenWindow opens the setup window and returns the code and the URLs to type it at.
func (s *Server) OpenWindow(now time.Time) (code string, urls []string, expires time.Time) {
	code, expires = s.Window.Open(now)
	select { // forget an older scan, then wait for SetupLoop to listen
	case <-s.scanned:
	default:
	}
	s.kickSetup()
	if s.loopRunning.Load() {
		select {
		case <-s.scanned:
		case <-time.After(3 * time.Second): // slow: advertise what's bound so far
		}
	}
	_, port, _ := net.SplitHostPort(s.Addr)
	addrs := s.boundAddrs()
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" && len(addrs) > 0 {
		h, _, _ = strings.Cut(h, ".") // mDNS name: pdb-print-pi.local
		urls = append(urls, "http://"+net.JoinHostPort(h+".local", port)+"/setup/")
	}
	slices.SortStableFunc(addrs, func(a, b netip.Addr) int { return cmp.Compare(a.BitLen(), b.BitLen()) }) // IPv4 first
	for _, a := range addrs {
		urls = append(urls, "http://"+net.JoinHostPort(a.String(), port)+"/setup/")
	}
	return code, urls, expires
}

// boundAddrs are the LAN addresses the setup server listens on right now.
func (s *Server) boundAddrs() []netip.Addr {
	s.boundMu.Lock()
	defer s.boundMu.Unlock()
	return slices.Clone(s.bound)
}

func (s *Server) setBound(m map[netip.Addr]bool) {
	s.boundMu.Lock()
	s.bound = s.bound[:0]
	for a := range m {
		s.bound = append(s.bound, a)
	}
	slices.SortFunc(s.bound, func(a, b netip.Addr) int { return a.Compare(b) })
	s.boundMu.Unlock()
}

func (s *Server) kickSetup() {
	select {
	case s.kick <- struct{}{}:
	default: // a kick is already pending
	}
}

func (s *Server) lanAddrs() []netip.Addr {
	if s.LANAddrs != nil {
		return s.LANAddrs()
	}
	return LocalAddrs()
}

// LocalAddrs are the host's addresses other computers can reach: up interfaces, no loopback,
// no link-local, IPv4 and global IPv6.
func LocalAddrs() []netip.Addr {
	var out []netip.Addr
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().IsGlobalUnicast() { // private IPv4 included
					out = append(out, ip.Unmap())
				}
			}
		}
	}
	return out
}

// StartSetupLoop runs SetupLoop in the background, marked as running before it returns (so a
// window opened right after waits for its first scan); done is closed when it has stopped.
func (s *Server) StartSetupLoop(ctx context.Context, tick <-chan time.Time) (done <-chan struct{}) {
	ch := make(chan struct{})
	s.loopRunning.Store(true)
	go func() {
		defer close(ch)
		s.SetupLoop(ctx, tick)
	}()
	return ch
}

// SetupLoop owns the LAN listeners: while the window is open it listens on every LAN address
// (rescanning on each tick, so Wi-Fi that comes up late is picked up); when it closes, the
// listeners and their connections are closed. Run it for the lifetime of serve.
func (s *Server) SetupLoop(ctx context.Context, tick <-chan time.Time) {
	s.loopRunning.Store(true)
	defer s.loopRunning.Store(false)
	var hs *http.Server
	bound := map[netip.Addr]bool{}
	failed := map[netip.Addr]bool{} // warned about once per window
	_, portS, _ := net.SplitHostPort(s.Addr)
	port, _ := strconv.ParseUint(portS, 10, 16) // 16 bits: the conversion below can't wrap
	stop := func() {
		if hs != nil {
			// let answers in flight (e.g. the "All set" page) go out, then drop the rest
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if hs.Shutdown(ctx) != nil {
				_ = hs.Close()
			}
			cancel()
			hs, bound, failed = nil, map[netip.Addr]bool{}, map[netip.Addr]bool{}
			s.setBound(bound)
			s.Log.Info("setup window closed")
		}
	}
	defer stop()
	for {
		if s.Window.IsOpen(time.Now()) {
			if hs == nil {
				hs = &http.Server{Handler: s.LANHandler(), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 2 * time.Minute}
			}
			for _, a := range s.lanAddrs() {
				if bound[a] {
					continue
				}
				ln, err := net.Listen("tcp", netip.AddrPortFrom(a, uint16(port)).String())
				if err != nil {
					if !failed[a] {
						s.Log.Warn("setup window: can't listen on this address; it won't be offered", "addr", a, "err", err)
						failed[a] = true
					}
					continue
				}
				bound[a] = true
				s.Log.Info("setup window listening", "addr", ln.Addr().String())
				go func(hs *http.Server) {
					if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
						s.Log.Warn("setup window listener stopped", "addr", a, "err", err)
					}
				}(hs)
			}
			s.setBound(bound)
		} else {
			stop()
		}
		select { // tell OpenWindow this scan is done
		case s.scanned <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-s.kick:
		}
	}
}

// LANHandler serves only the setup wizard, to browsers that typed the code.
func (s *Server) LANHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /setup/{$}", s.setupStart)
	mux.HandleFunc("POST /setup/pair", s.pair)
	s.setupRoutes(mux)
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	icons, _ := fs.Sub(assets.Web, "icon/web")
	for _, f := range []string{"favicon.svg", "favicon.ico", "apple-touch-icon.png"} {
		mux.Handle("GET /"+f, http.FileServerFS(icons))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		now := time.Now()
		if !s.lanClient(r.RemoteAddr) {
			http.Error(w, "setup is only open to the local network", http.StatusForbidden)
			return
		}
		if !s.lanHost(r.Host) { // against DNS rebinding: only our own addresses and mDNS name
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		sess, ok := s.Window.Session(r, now)
		open := r.URL.Path == "/setup/" || r.URL.Path == "/setup/pair" || !isSetupPath(r.URL.Path)
		switch {
		case !ok && !open && r.Method == http.MethodPost:
			http.Error(w, "the setup window is closed or this browser isn't paired: reload the page", http.StatusForbidden)
			return
		case !ok && !open:
			http.Redirect(w, r, "/setup/", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if !ok && r.URL.Path != "/setup/pair" {
				http.Error(w, "this browser isn't paired: reload the page", http.StatusForbidden)
				return
			}
			if ok && subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(sess.CSRF)) != 1 {
				http.Error(w, "invalid or missing CSRF token: reload the page", http.StatusForbidden)
				return
			}
		}
		if ok {
			s.Window.Touch(now)
			r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess))
		}
		mux.ServeHTTP(w, r)
	})
}

// lanClient reports whether a request comes from the local network: a private, unique-local,
// link-local or loopback address, or the IPv6 /64 of one of our interfaces (a home LAN's public
// prefix, which a browser opening <name>.local often uses). Public IPv4 subnets are never trusted:
// on a WAN interface they hold the provider's other customers.
func (s *Server) lanClient(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	for _, n := range s.lanNets() {
		if n.Addr().Is6() && n.Bits() >= 64 && n.Masked().Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) lanNets() []netip.Prefix {
	if s.LANNets != nil {
		return s.LANNets()
	}
	return LocalNets()
}

// LocalNets are the subnets of the host's up, non-loopback interfaces, with their real prefix length.
func LocalNets() []netip.Prefix {
	var out []netip.Prefix
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				ip, ok := netip.AddrFromSlice(n.IP)
				bits, _ := n.Mask.Size()
				if ok && ip.Unmap().IsGlobalUnicast() {
					if ip.Unmap().Is4() && bits > 32 {
						bits -= 96 // IPv4 in a 16-byte mask
					}
					out = append(out, netip.PrefixFrom(ip.Unmap(), bits))
				}
			}
		}
	}
	return out
}

// lanHost reports whether host (host:port from the request) names this machine on the LAN.
func (s *Server) lanHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return slices.Contains(s.lanAddrs(), a.Unmap())
	}
	name, _ := os.Hostname()
	name, _, _ = strings.Cut(name, ".")
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	return name != "" && (h == strings.ToLower(name) || h == strings.ToLower(name)+".local")
}

func isSetupPath(p string) bool { return len(p) >= len("/setup/") && p[:len("/setup/")] == "/setup/" }

type sessionKey struct{}

// session returns the wizard session of a request (LAN: from the cookie; loopback: see setupRoutes).
func session(r *http.Request) *Session {
	s, _ := r.Context().Value(sessionKey{}).(*Session)
	return s
}

// setupStart shows the pairing page, or sends a paired browser into the wizard.
func (s *Server) setupStart(w http.ResponseWriter, r *http.Request) {
	if session(r) != nil {
		http.Redirect(w, r, "/setup/partdb", http.StatusSeeOther)
		return
	}
	s.pairPage(w, "", http.StatusOK)
}

func (s *Server) pairPage(w http.ResponseWriter, msg string, status int) {
	open := s.Window.IsOpen(time.Now())
	mins := int(time.Until(s.Window.Expires()).Round(time.Minute) / time.Minute)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pages.ExecuteTemplate(w, "pair.html", map[string]any{"Open": open, "Minutes": max(mins, 1), "Err": msg, "Version": s.Version})
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.Window.Check(r.FormValue("code"), time.Now())
	if !ok && s.Window.CodeUsed() && s.Window.IsOpen(time.Now()) {
		s.Log.Warn("setup window: pairing code already used", "from", r.RemoteAddr)
		s.pairPage(w, "This code has already been used: it pairs one browser. For another browser, run "+
			"\"pdb-print pair\" on the print server; it opens the window with a new code.", http.StatusForbidden)
		return
	}
	if !ok {
		s.Log.Warn("setup window: wrong pairing code", "from", r.RemoteAddr)
		s.pairPage(w, "That code didn't match. Check the code and try again.", http.StatusForbidden)
		return
	}
	s.Log.Info("setup window: browser paired", "from", r.RemoteAddr)
	// Secure only over TLS: the setup window is plain HTTP on the LAN, where a Secure cookie would
	// never come back and pairing would fail. SameSite=Strict and HttpOnly hold either way.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sess.ID, Path: "/setup", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	http.Redirect(w, r, "/setup/partdb", http.StatusSeeOther)
}

// pairAPI is POST /v1/pair on the loopback server: `pdb-print pair` opens the window with it.
func (s *Server) pairAPI(w http.ResponseWriter, r *http.Request) {
	code, urls, expires := s.OpenWindow(time.Now())
	s.Log.Info("setup window opened", "code", code, "urls", urls, "until", expires.Format("15:04"))
	writeJSON(w, map[string]any{"code": code, "urls": urls, "expires": expires})
}

// loopbackRemote reports whether a request came from this machine.
func loopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip, perr := netip.ParseAddr(host)
	return err == nil && perr == nil && ip.Unmap().IsLoopback()
}
