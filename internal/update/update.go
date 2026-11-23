// Package update tells whether a newer pdb-print release is out. It only asks GitHub for the latest
// release's tag (no data about this agent is sent). An installed Windows copy updates itself from the
// web UI (internal/selfupdate); elsewhere updating stays with the user's package manager.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/francisrath/pdb-print/internal/wininstall"
)

const (
	latestURL = "https://api.github.com/repos/francisrath/pdb-print/releases/latest"
	// Every is how often the agent asks.
	Every = 24 * time.Hour
)

// Release is a newer release than the running one.
type Release struct {
	Version string // "0.3.0"
	URL     string // its release page
	Hint    string // how to update on this platform
}

type Checker struct {
	Current string       // the running version ("0.2.0"); anything but a plain release is never checked
	URL     string       // "" = GitHub's latest-release endpoint
	Client  *http.Client // nil = 15 s timeout
	Log     *slog.Logger // nil = no logging
	// SelfUpdate: this Windows install updates itself (the web UI's Update now button); only for
	// Release.Hint, which the log shows (the web UI computes its hint when it renders).
	SelfUpdate bool

	mu     sync.Mutex
	latest *Release
}

var releaseRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// Released: v is a plain release version (not "dev", a snapshot or a pseudo-version).
func Released(v string) bool { return releaseRe.MatchString(v) }

// newer: a is a later release than b (both plain releases).
func newer(a, b string) bool {
	ma, mb := releaseRe.FindStringSubmatch(a), releaseRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// Check asks once; it records and returns the newer release, or nil when the running one is current.
func (c *Checker) Check(ctx context.Context) (*Release, error) {
	url, client := c.URL, c.Client
	if url == "" {
		url = latestURL
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pdb-print")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("latest release: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Tag string `json:"tag_name"`
		URL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("latest release: %w", err)
	}
	var rel *Release
	if newer(r.Tag, c.Current) {
		v := strings.TrimPrefix(r.Tag, "v")
		rel = &Release{Version: v, URL: r.URL, Hint: Hint(v, c.SelfUpdate)}
	}
	c.mu.Lock()
	c.latest = rel
	c.mu.Unlock()
	return rel, nil
}

// Latest is the newer release the last check found (nil = none, or not checked yet).
func (c *Checker) Latest() *Release {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run checks now and then every Every until ctx ends, logging a newer release once per version.
// It does nothing for a version that isn't a plain release.
func (c *Checker) Run(ctx context.Context) {
	if !Released(c.Current) {
		return
	}
	logged := ""
	for {
		rel, err := c.Check(ctx)
		switch {
		case err != nil && ctx.Err() == nil && c.Log != nil:
			c.Log.Debug("update check failed", "err", err)
		case rel != nil && rel.Version != logged && c.Log != nil:
			c.Log.Info("a newer pdb-print is available", "version", rel.Version, "running", c.Current, "notes", rel.URL, "update", rel.Hint)
			logged = rel.Version
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(Every):
		}
	}
}

// Install is how this copy was installed: deb, brew, docker, windows (in a `pdb-print install`
// program dir), zip (other Windows copies) or archive.
func Install() string {
	exe, _ := os.Executable()
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real // brew links /opt/homebrew/bin/pdb-print into its Caskroom
	}
	_, installed := wininstall.ModeOf(exe, wininstall.EnvFromOS())
	return install(runtime.GOOS, exe, fileExists("/.dockerenv"), installed)
}

func install(goos, exe string, docker, winInstalled bool) string {
	switch {
	case goos == "darwin" && strings.Contains(exe, "/Caskroom/"):
		return "brew"
	case goos == "windows" && winInstalled:
		return "windows"
	case goos == "windows":
		return "zip"
	case docker:
		return "docker"
	case exe == "/usr/bin/pdb-print": // the .deb
		return "deb"
	}
	return "archive"
}

// Hint says how to update this install to version v; selfUpdate: it updates itself (the button).
func Hint(v string, selfUpdate bool) string { return hint(Install(), v, selfUpdate) }

// hint says how to update an install of kind how to version v.
func hint(how, v string, selfUpdate bool) string {
	switch how {
	case "windows":
		if selfUpdate {
			return "click Update now in the web UI"
		}
		return "download pdb-print_" + v + "_windows_amd64_setup.exe from the release page and run it"
	case "brew":
		return "brew upgrade --cask pdb-print"
	case "zip":
		return "close the window pdb-print runs in, download pdb-print_" + v + "_windows_amd64.zip from the release page, extract it and double-click pdb-print.exe to install it"
	case "docker":
		return "pull the new image (ghcr.io/francisrath/pdb-print:" + v + ") and recreate the container"
	case "deb":
		return "download the .deb for this machine from the release page, then: sudo apt install ./pdb-print_" + v + "_<arch>.deb"
	}
	exe, _ := os.Executable()
	return "download the archive for this machine from the release page and replace " + exe
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
