package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// browser B has no cookie: pairing page, and its wrong tries count towards the same 5
	if w := serveLAN(lan, lanReq("GET", "/setup/", nil, nil)); !strings.Contains(w.Body.String(), "6-digit") {
		t.Errorf("browser B without code: %d %s", w.Code, w.Body)
	}
	for range 5 {
		if pairLAN(lan, wrongCode(code)) != nil {
			t.Fatal("wrong code paired")
		}
	}
	if h.srv.Window.IsOpen(time.Now()) {
		t.Error("window open after 5 wrong codes from browser B")
	}
}

func TestLANGuard(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.srv.OpenWindow(time.Now())
	lan := h.srv.LANHandler()
	for _, p := range []string{"/", "/printers/ql/status", "/v1/info", "/v1/handoff"} {
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
	buf := make([]byte, 1<<16)
	if n, err := conn.Read(buf); err != nil || !strings.Contains(string(buf[:n]), "200 OK") {
		t.Fatalf("pairing page over TCP: %q %v", buf[:n], err)
	}

	h.srv.Window.Close()
	eventually(t, "LAN listener still up after Close", func() bool { return !reachable(addr) })
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Error("open connection survived the window")
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
