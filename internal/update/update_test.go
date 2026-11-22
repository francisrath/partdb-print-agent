package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.3.0", "0.2.0", true},
		{"v0.2.10", "0.2.9", true}, // numeric, not string order
		{"v1.0.0", "0.9.9", true},
		{"v0.2.0", "0.2.0", false},
		{"v0.1.9", "0.2.0", false},
		{"v0.3.0-rc.1", "0.2.0", false}, // pre-releases aren't offered
		{"v0.3.0", "dev", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestReleased(t *testing.T) {
	for v, want := range map[string]bool{
		"0.2.0": true, "v0.2.0": true, "dev": false, "0.1.1-SNAPSHOT-94134d1": false,
		"v0.1.2-0.20261119121909-9e0476ab6b03": false, "": false,
	} {
		if Released(v) != want {
			t.Errorf("Released(%q) = %v", v, !want)
		}
	}
}

func TestCheck(t *testing.T) {
	tag := "v0.3.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "pdb-print" {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://example.test/v0.3.0","body":"…"}`))
	}))
	defer srv.Close()
	c := &Checker{Current: "0.2.0", URL: srv.URL}
	if c.Latest() != nil {
		t.Fatal("latest before any check")
	}
	rel, err := c.Check(context.Background())
	if err != nil || rel == nil || rel.Version != "0.3.0" || rel.URL != "https://example.test/v0.3.0" || rel.Hint == "" || c.Latest() != rel {
		t.Fatalf("%+v, %v", rel, err)
	}
	tag = "v0.2.0" // the user updated, or the release was pulled
	if rel, err := c.Check(context.Background()); err != nil || rel != nil || c.Latest() != nil {
		t.Errorf("current: %+v, %v; latest %+v", rel, err, c.Latest())
	}
	var nilChecker *Checker
	if nilChecker.Latest() != nil {
		t.Error("nil checker")
	}
}

func TestCheckErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"rate limited": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "slow down", http.StatusForbidden) },
		"garbage":      func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	} {
		srv := httptest.NewServer(h)
		c := &Checker{Current: "0.2.0", URL: srv.URL}
		if _, err := c.Check(context.Background()); err == nil {
			t.Errorf("%s: no error", name)
		}
		srv.Close()
	}
}

// A dev build never asks.
func TestRunSkipsDevBuilds(t *testing.T) {
	asked := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked = true }))
	defer srv.Close()
	// a live context: a cancelled one would stop the request even without the Released guard
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	(&Checker{Current: "dev", URL: srv.URL}).Run(ctx)
	if asked {
		t.Error("dev build asked GitHub")
	}
}

func TestInstall(t *testing.T) {
	for _, c := range []struct {
		goos, exe         string
		docker, installed bool
		want              string
	}{
		{"darwin", "/opt/homebrew/Caskroom/pdb-print/0.2.0/pdb-print", false, false, "brew"},
		{"darwin", "/usr/local/Caskroom/pdb-print/0.2.0/pdb-print", false, false, "brew"}, // Intel Homebrew
		{"darwin", "/Users/me/bin/pdb-print", false, false, "archive"},
		{"windows", `C:\pdb\pdb-print.exe`, false, false, "zip"},
		{"linux", "/usr/bin/pdb-print", true, false, "docker"},
		{"linux", "/usr/bin/pdb-print", false, false, "deb"},
		{"linux", "/usr/local/bin/pdb-print", false, false, "archive"},
	} {
		if got := install(c.goos, c.exe, c.docker, c.installed); got != c.want {
			t.Errorf("%s %s docker=%v: %s, want %s", c.goos, c.exe, c.docker, got, c.want)
		}
	}
}

// Only Homebrew copies get the brew command.
func TestHintFollowsInstall(t *testing.T) {
	h := hint(Install(), "9.9.9", false)
	if (Install() == "brew") != strings.Contains(h, "brew upgrade") {
		t.Errorf("install %s, hint %q", Install(), h)
	}
}

// A copy in a wininstall program dir is "windows"; it says to click the button only when it updates itself.
func TestInstallWindows(t *testing.T) {
	for _, c := range []struct {
		installed, selfUpdate bool
		install, hint         string
	}{
		{false, true, "zip", "extract it and double-click pdb-print.exe"},
		{true, true, "windows", "click Update now in the web UI"},
		{true, false, "windows", "pdb-print_0.3.0_windows_amd64_setup.exe"},
	} {
		got := install("windows", `C:\Users\me\AppData\Local\Programs\pdb-print\pdb-print.exe`, false, c.installed)
		if h := hint(got, "0.3.0", c.selfUpdate); got != c.install || !strings.Contains(h, c.hint) {
			t.Errorf("installed=%v self_update=%v: %s, %q; want %s, %q", c.installed, c.selfUpdate, got, h, c.install, c.hint)
		}
	}
}
