package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// wrongCode is a 6-digit code that isn't code.
func wrongCode(code string) string {
	if code == "000000" {
		return "111111"
	}
	return "000000"
}

func TestPairCode(t *testing.T) {
	var w Window
	code, expires := w.Open(t0)
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) || !expires.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("code %q expires %v", code, expires)
	}
	if _, ok := w.Check(wrongCode(code), t0); ok {
		t.Error("wrong code accepted")
	}
	s, ok := w.Check(code, t0)
	if !ok || len(s.ID) < 32 || len(s.CSRF) < 32 || s.ID == s.CSRF {
		t.Fatalf("session %+v %v", s, ok)
	}
}

func TestWrongCodesClose(t *testing.T) {
	var w Window
	code, _ := w.Open(t0)
	for range 5 {
		w.Check(wrongCode(code), t0)
	}
	if w.IsOpen(t0) {
		t.Error("still open after 5 wrong codes")
	}
	if _, ok := w.Check(code, t0); ok {
		t.Error("right code accepted after the window closed")
	}
}

func TestWindowExpiryAndTouch(t *testing.T) {
	var w Window
	w.Open(t0)
	if !w.IsOpen(t0.Add(14 * time.Minute)) {
		t.Fatal("closed before 15 minutes")
	}
	if w.IsOpen(t0.Add(15*time.Minute + time.Second)) {
		t.Fatal("open after 15 minutes without use")
	}

	w.Open(t0)
	w.Touch(t0.Add(12 * time.Minute)) // in use: at least 5 more minutes
	if !w.IsOpen(t0.Add(16 * time.Minute)) {
		t.Fatal("Touch didn't extend")
	}
	for m := 16; m < 30; m++ {
		w.Touch(t0.Add(time.Duration(m) * time.Minute))
	}
	if !w.IsOpen(t0.Add(30 * time.Minute)) {
		t.Fatal("closed before the 30 minute cap")
	}
	if w.IsOpen(t0.Add(30*time.Minute + time.Second)) {
		t.Fatal("open beyond 30 minutes")
	}
}

// lanReq builds a request as it arrives on the LAN listener.
func lanReq(method, path string, form url.Values, cookie *http.Cookie) *http.Request {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "http://192.168.1.23:8631"+path, body)
	r.RemoteAddr = "192.168.1.40:50000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func serveLAN(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// pairLAN enters code from a LAN browser and returns its session cookie (nil when refused).
func pairLAN(h http.Handler, code string) *http.Cookie {
	w := serveLAN(h, lanReq("POST", "/setup/pair", url.Values{"code": {code}}, nil))
	for _, c := range w.Result().Cookies() {
		if c.Name == "pdb_setup" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestSecondBrowserNeedsCode(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	a := pairLAN(lan, code)
	if a == nil || !a.HttpOnly || a.SameSite != http.SameSiteStrictMode {
		t.Fatalf("browser A cookie %+v", a)
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/", nil, a)); strings.Contains(w.Body.String(), "6-digit") {
		t.Error("paired browser still sees the pairing page")
	}
	// browser B has no cookie: the pairing page; the code is used up, so neither it nor guesses pair,
	// and B's tries don't close the window on A (nothing is left to guess once the code is used)
	if w := serveLAN(lan, lanReq("GET", "/setup/", nil, nil)); !strings.Contains(w.Body.String(), "6-digit") {
		t.Errorf("browser B without code: %d %s", w.Code, w.Body)
	}
	if pairLAN(lan, code) != nil {
		t.Fatal("browser B paired with the code A already used")
	}
	w := serveLAN(lan, lanReq("POST", "/setup/pair", url.Values{"code": {code}}, nil))
	if !strings.Contains(w.Body.String(), "already been used") || !strings.Contains(w.Body.String(), "pdb-print pair") {
		t.Errorf("used code: %d %s", w.Code, w.Body)
	}
	for range 5 {
		if pairLAN(lan, wrongCode(code)) != nil {
			t.Fatal("wrong code paired")
		}
	}
	if !h.srv.Window.IsOpen(time.Now()) {
		t.Error("browser B's tries closed the window on browser A")
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/", nil, a)); strings.Contains(w.Body.String(), "6-digit") {
		t.Error("browser A lost its session")
	}
}

// The code pairs one browser; opening the window again makes a new one.
func TestCodeIsSingleUse(t *testing.T) {
	var w Window
	code, _ := w.Open(t0)
	if _, ok := w.Check(code, t0); !ok {
		t.Fatal("first use refused")
	}
	if !w.CodeUsed() {
		t.Error("CodeUsed false after pairing")
	}
	if _, ok := w.Check(code, t0); ok {
		t.Error("code accepted twice")
	}
	code2, _ := w.Open(t0)
	if w.CodeUsed() {
		t.Error("new window starts with a used code")
	}
	if _, ok := w.Check(code2, t0); !ok {
		t.Error("new code refused")
	}
}

func TestLANGuard(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	for _, p := range []string{"/printers/ql/status", "/v1/info", "/v1/handoff"} {
		if w := serveLAN(lan, lanReq("GET", p, nil, nil)); w.Code != http.StatusNotFound {
			t.Errorf("GET %s on the LAN: %d", p, w.Code)
		}
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/", nil, nil)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "6-digit") {
		t.Errorf("pairing page: %d", w.Code)
	}
	r := lanReq("POST", "/setup/pair", url.Values{"code": {code}}, nil)
	r.Header.Set("Origin", "http://evil.example")
	if w := serveLAN(lan, r); w.Code != http.StatusForbidden {
		t.Errorf("foreign Origin on pair: %d", w.Code)
	}
	c := pairLAN(lan, code)
	if c == nil {
		t.Fatal("not paired")
	}
	if w := serveLAN(lan, lanReq("POST", "/setup/partdb", url.Values{}, c)); w.Code != http.StatusForbidden {
		t.Errorf("POST without the session's CSRF token: %d", w.Code)
	}
	if w := serveLAN(lan, lanReq("POST", "/setup/partdb", url.Values{"csrf": {h.srv.csrf}}, c)); w.Code != http.StatusForbidden {
		t.Errorf("POST with the loopback CSRF token: %d", w.Code)
	}
	if w := serveLAN(lan, lanReq("GET", "/setup/partdb", nil, nil)); w.Code != http.StatusSeeOther {
		t.Errorf("wizard page without a session: %d", w.Code)
	}
}

func TestPairLoopbackOnly(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name   string
		remote string
		origin string
		want   int
	}{
		{"loopback", "127.0.0.1:50000", "", http.StatusOK},
		{"from the LAN", "192.168.1.40:50000", "", http.StatusForbidden},
		{"from a web page", "127.0.0.1:50000", "http://127.0.0.1:8631", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do("POST", "/v1/pair", url.Values{}, func(r *http.Request) {
				r.RemoteAddr = tc.remote
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
			})
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if tc.want != http.StatusOK {
				return
			}
			var res struct {
				Code    string
				URLs    []string
				Expires time.Time
			}
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Code) != 6 || res.Expires.IsZero() {
				t.Errorf("reply %s: %v", w.Body, err)
			}
		})
	}
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// eventually retries f for up to 2 s.
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatal(what)
}

func reachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// startLAN runs the setup loop with the given "LAN" addresses and a manual tick.
func startLAN(t *testing.T, h *harness, addrs func() []netip.Addr) (port int, tick chan time.Time) {
	port = freePort(t)
	h.srv.Addr = "127.0.0.1:" + strconv.Itoa(port) // the loopback server isn't listening in tests
	h.srv.LANAddrs = addrs
	tick = make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); h.srv.SetupLoop(ctx, tick) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	eventually(t, "SetupLoop not started", h.srv.loopRunning.Load)
	return port, tick
}

func TestWindowCloseStopsLAN(t *testing.T) {
	h := newHarness(t)
	lo := netip.MustParseAddr("127.0.0.1")
	port, _ := startLAN(t, h, func() []netip.Addr { return []netip.Addr{lo} })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if reachable(addr) {
		t.Fatal("LAN listener up before the window opened")
	}
	h.srv.OpenWindow(time.Now())
	eventually(t, "LAN listener not up after OpenWindow", func() bool { return reachable(addr) })

	conn, err := net.Dial("tcp", addr) // an open keep-alive connection
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /setup/ HTTP/1.1\r\nHost: " + addr + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("pairing page over TCP: %v %v", resp, err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil { // the whole answer: nothing left unread
		t.Fatal(err)
	}

	h.srv.Window.Close()
	eventually(t, "LAN listener still up after Close", func() bool { return !reachable(addr) })
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = br.ReadByte()
	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("open connection survived the window: %v", err)
	}
	if w := h.do("GET", "/", nil); w.Code != http.StatusOK {
		t.Errorf("loopback UI after close: %d", w.Code)
	}
}

func TestRescanAddsAddress(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	addrs := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	port, tick := startLAN(t, h, func() []netip.Addr { mu.Lock(); defer mu.Unlock(); return slices.Clone(addrs) })
	h.srv.OpenWindow(time.Now())
	v6 := net.JoinHostPort("::1", strconv.Itoa(port))
	eventually(t, "IPv4 listener not up", func() bool { return reachable(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) })
	if reachable(v6) {
		t.Skip("::1 port already in use")
	}
	mu.Lock()
	addrs = append(addrs, netip.MustParseAddr("::1")) // Wi-Fi came up
	mu.Unlock()
	tick <- time.Now()
	eventually(t, "no listener for the new address after a rescan", func() bool { return reachable(v6) })
}

// POSTs from a browser without a session are refused, not crashing the handler.
func TestLANPostWithoutSession(t *testing.T) {
	h := newHarness(t)
	h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	for _, p := range []string{"/setup/", "/favicon.svg", "/static/style.css"} {
		if w := serveLAN(lan, lanReq("POST", p, url.Values{}, nil)); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without a session: %d", p, w.Code)
		}
	}
}

// A page on another site that rebinds its name to the print server can't use the wizard:
// the LAN server only answers to its own addresses and mDNS name.
func TestLANRejectsForeignHost(t *testing.T) {
	h := newHarness(t)
	h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	r := lanReq("GET", "/setup/", nil, nil)
	r.Host = "evil.example:8631"
	if w := serveLAN(lan, r); w.Code != http.StatusForbidden {
		t.Errorf("foreign Host: %d", w.Code)
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	r = lanReq("GET", "/setup/", nil, nil)
	r.Host = host + ".local:8631"
	if w := serveLAN(lan, r); w.Code != http.StatusOK {
		t.Errorf("mDNS name %s.local: %d", host, w.Code)
	}
}

// Only addresses the setup server could listen on are advertised; one that can't be bound
// (here: not this machine's) is left out.
func TestPairAdvertisesBoundOnly(t *testing.T) {
	h := newHarness(t)
	startLAN(t, h, func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.77")}
	})
	_, urls, _ := h.srv.OpenWindow(time.Now())
	all := strings.Join(urls, " ")
	if !strings.Contains(all, "127.0.0.1") || strings.Contains(all, "192.0.2.77") {
		t.Errorf("urls %v", urls)
	}
}

// The wizard answers only clients on the local network, even on a public address.
func TestLANRejectsInternetClients(t *testing.T) {
	h := newHarness(t)
	h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	for remote, want := range map[string]int{
		"203.0.113.5:4000":    http.StatusForbidden, // the internet
		"[2001:db8::5]:4000":  http.StatusForbidden,
		"192.168.1.40:4000":   http.StatusOK, // private
		"[fd00::5]:4000":      http.StatusOK, // ULA
		"[fe80::1%eth0]:4000": http.StatusOK, // link-local
	} {
		r := lanReq("GET", "/setup/", nil, nil)
		r.RemoteAddr = remote
		if w := serveLAN(lan, r); w.Code != want {
			t.Errorf("client %s: %d, want %d", remote, w.Code, want)
		}
	}
}

// Public IPv4 subnets are never trusted (a WAN interface's /24 holds other customers); a public
// IPv6 /64 is the host's own link (a home LAN's prefix, which browsers on it use).
func TestLANClientsUseRealSubnets(t *testing.T) {
	h := newHarness(t)
	h.srv.LANNets = func() []netip.Prefix {
		return []netip.Prefix{netip.MustParsePrefix("198.51.100.10/24"), netip.MustParsePrefix("2001:db8:1::10/64")}
	}
	h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	for remote, want := range map[string]int{
		"198.51.100.20:4000":    http.StatusForbidden, // a WAN /24: other customers of the provider
		"[2001:db8:1::20]:4000": http.StatusOK,        // same /64 link
		"[2001:db8:2::20]:4000": http.StatusForbidden,
	} {
		r := lanReq("GET", "/setup/", nil, nil)
		r.RemoteAddr = remote
		if w := serveLAN(lan, r); w.Code != want {
			t.Errorf("client %s: %d, want %d", remote, w.Code, want)
		}
	}
}

// A window opened right as serve starts (--setup-window) still lists its addresses in the log.
func TestOpenWindowRightAfterStart(t *testing.T) {
	h := newHarness(t)
	h.srv.Addr = "127.0.0.1:" + strconv.Itoa(freePort(t))
	h.srv.LANAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }
	ctx, cancel := context.WithCancel(context.Background())
	done := h.srv.StartSetupLoop(ctx, make(chan time.Time))
	t.Cleanup(func() { cancel(); <-done })
	if _, urls, _ := h.srv.OpenWindow(time.Now()); !strings.Contains(strings.Join(urls, " "), "127.0.0.1") {
		t.Errorf("urls %v", urls)
	}
}

// On the LAN, / leads to the setup page.
func TestLANRootRedirectsToSetup(t *testing.T) {
	h := newHarness(t)
	h.srv.OpenWindow(time.Now())
	w := serveLAN(h.srv.LANHandler(), lanReq("GET", "/", nil, nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup/" {
		t.Errorf("GET / on the LAN: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// Finish closes the window a moment later, so the "All set" page can still load its stylesheet;
// a window reopened meanwhile (pdb-print pair) stays open.
func TestFinishClosesAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.srv.finishGrace = 50 * time.Millisecond
	code, _, _ := h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	c := pairLAN(lan, code)
	sess, _ := h.srv.Window.Session(lanReq("GET", "/setup/", nil, c), time.Now())
	sess.Draft.Saved = []string{"Desk"}
	serveLAN(lan, lanReq("POST", "/setup/done", url.Values{"action": {"finish"}, "csrf": {sess.CSRF}}, c))
	if !h.srv.Window.IsOpen(time.Now()) {
		t.Fatal("closed before the page could load its stylesheet")
	}
	eventually(t, "window still open after the grace period", func() bool { return !h.srv.Window.IsOpen(time.Now()) })

	code, _, _ = h.srv.OpenWindow(time.Now())
	c = pairLAN(lan, code)
	sess, _ = h.srv.Window.Session(lanReq("GET", "/setup/", nil, c), time.Now())
	sess.Draft.Saved = []string{"Desk"}
	serveLAN(lan, lanReq("POST", "/setup/done", url.Values{"action": {"finish"}, "csrf": {sess.CSRF}}, c))
	h.srv.OpenWindow(time.Now()) // pdb-print pair right after Finish
	time.Sleep(150 * time.Millisecond)
	if !h.srv.Window.IsOpen(time.Now()) {
		t.Error("Finish's delayed close shut a window opened after it")
	}
}

// Finish closes the window its session was paired in, even if another opened just before.
func TestFinishClosesItsOwnWindow(t *testing.T) {
	h := newHarness(t)
	h.srv.finishGrace = 20 * time.Millisecond
	code, _, _ := h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	c := pairLAN(lan, code)
	sess, _ := h.srv.Window.Session(lanReq("GET", "/setup/", nil, c), time.Now())
	if sess.Gen != h.srv.Window.Generation() {
		t.Fatalf("session generation %d, window %d", sess.Gen, h.srv.Window.Generation())
	}
	sess.Draft.Saved = []string{"Desk"}
	sess.Gen-- // as if `pdb-print pair` had opened a new window between pairing and Finish
	serveLAN(lan, lanReq("POST", "/setup/done", url.Values{"action": {"finish"}, "csrf": {sess.CSRF}}, c))
	time.Sleep(100 * time.Millisecond)
	if !h.srv.Window.IsOpen(time.Now()) {
		t.Error("Finish closed a window it wasn't paired in")
	}
}

func TestQuitLoopbackWithHeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		name, remote, header string
		want                 int
	}{
		{"ok", "127.0.0.1:50000", "1", http.StatusAccepted},
		{"header missing", "127.0.0.1:50000", "", http.StatusForbidden},
		{"from the LAN", "192.168.1.40:50000", "1", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			quit := 0
			h.srv.Quit = func() { quit++ }
			w := h.do("POST", "/v1/quit", nil, func(r *http.Request) {
				r.RemoteAddr = tc.remote
				if tc.header != "" {
					r.Header.Set("X-PDB-Print", tc.header)
				}
			})
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if wantQuit := btoi(tc.want == http.StatusAccepted); quit != wantQuit {
				t.Errorf("Quit called %d times, want %d", quit, wantQuit)
			}
			if tc.want == http.StatusAccepted && strings.TrimSpace(w.Body.String()) != fmt.Sprintf(`{"pid":%d}`, os.Getpid()) {
				t.Errorf("body %q, want our pid", w.Body)
			}
		})
	}
}

func TestQuitNotOfferedWithoutQuit(t *testing.T) {
	h := newHarness(t) // Quit unset, as serve leaves it outside Windows
	w := h.do("POST", "/v1/quit", nil, func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:50000"
		r.Header.Set("X-PDB-Print", "1")
	})
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d: %s", w.Code, w.Body)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A window whose code paired a browser doesn't count toward the one-per-minute limit (only wrong
// codes and expiry do): "code already used, run pdb-print pair" works at once.
func TestOpenOrCurrentAfterPairing(t *testing.T) {
	var w Window
	code, _ := w.Open(t0)
	if _, ok := w.Check(code, t0); !ok {
		t.Fatal("code didn't pair")
	}
	if c2, _, err := w.OpenOrCurrent(t0.Add(time.Second)); err != nil || c2 == code {
		t.Fatalf("used, still open: %q %v", c2, err)
	}
	c3, _, _ := w.OpenOrCurrent(t0.Add(2 * time.Second)) // the open, unused one
	if _, ok := w.Check(c3, t0.Add(3*time.Second)); !ok {
		t.Fatal("code didn't pair")
	}
	w.Close() // e.g. Finish
	if _, _, err := w.OpenOrCurrent(t0.Add(4 * time.Second)); err != nil {
		t.Fatalf("closed after pairing: %v", err)
	}
	w.Close() // unused: closed as by wrong codes
	if _, _, err := w.OpenOrCurrent(t0.Add(5 * time.Second)); !errors.Is(err, errTooSoon) {
		t.Fatalf("closed unused: %v", err)
	}
}

// Asking again for an open, unused window gives it a full window from now.
func TestOpenOrCurrentExtends(t *testing.T) {
	var w Window
	code, _ := w.Open(t0)
	later := t0.Add(25 * time.Minute)
	w.Touch(t0.Add(12 * time.Minute)) // in use: open until 17 min
	again, expires, err := w.OpenOrCurrent(t0.Add(16 * time.Minute))
	if err != nil || again != code || !expires.Equal(t0.Add(31*time.Minute)) {
		t.Fatalf("%q %v %v, want %q until %v", again, expires, err, code, t0.Add(31*time.Minute))
	}
	w.Touch(later) // the 30-minute cap counts from the request
	if !w.IsOpen(t0.Add(31 * time.Minute)) {
		t.Error("Touch shortened the window")
	}
}
