// Package wininstall installs pdb-print on Windows: program files, autostart,
// Start-menu link, "Apps & features" entry and (later) the service. All
// registry/service/process access goes through System so the logic is testable
// anywhere.
package wininstall

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	name     = "pdb-print"
	exeName  = "pdb-print.exe"
	exeWName = "pdb-printw.exe"
	uiURL    = "http://127.0.0.1:8631"
)

// ErrNotWindows is returned by the System on non-Windows platforms.
var ErrNotWindows = errors.New("wininstall: only supported on Windows")

// Mode is the install flavour.
type Mode int

const (
	PerUser Mode = iota
	Service
)

// Env holds the Windows folder locations (from environment variables).
type Env struct{ LocalAppData, AppData, ProgramFiles, ProgramData, StartMenuUser, StartMenuAll string }

const startMenuPrograms = `Microsoft\Windows\Start Menu\Programs`

// EnvFromOS reads the locations from the process environment.
func EnvFromOS() Env {
	e := Env{
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		AppData:      os.Getenv("APPDATA"),
		ProgramFiles: os.Getenv("ProgramFiles"),
		ProgramData:  os.Getenv("ProgramData"),
	}
	e.StartMenuUser = filepath.Join(e.AppData, startMenuPrograms)
	e.StartMenuAll = filepath.Join(e.ProgramData, startMenuPrograms)
	return e
}

// Layout is where everything of one install mode lives.
type Layout struct {
	Mode                                         Mode
	ProgramDir, Exe, ExeW, Config, StartMenuLink string
}

// LayoutFor computes the paths for mode m.
func LayoutFor(m Mode, e Env) Layout {
	l := Layout{Mode: m}
	if m == Service {
		l.ProgramDir = filepath.Join(e.ProgramFiles, name)
		l.Config = filepath.Join(e.ProgramData, name, "config.yaml")
		l.StartMenuLink = filepath.Join(e.StartMenuAll, name+".url")
	} else {
		l.ProgramDir = filepath.Join(e.LocalAppData, "Programs", name)
		l.Config = filepath.Join(e.AppData, name, "config.yaml")
		l.StartMenuLink = filepath.Join(e.StartMenuUser, name+".url")
	}
	l.Exe = filepath.Join(l.ProgramDir, exeName)
	l.ExeW = filepath.Join(l.ProgramDir, exeWName)
	return l
}

// Entry is the "Apps & features" uninstall entry.
type Entry struct{ DisplayName, DisplayVersion, Publisher, InstallLocation, UninstallString string }

// System is everything that touches the registry, services and processes.
type System interface {
	IsAdmin() bool
	SetRun(name, cmdline string) error
	DeleteRun(name string) error
	WriteUninstall(m Mode, e Entry) error
	ReadUninstall(m Mode) (Entry, bool, error)
	DeleteUninstall(m Mode) error
	ServiceExists() (bool, error)
	InstallService(exe string, args []string) error
	RemoveService() error
	StartService() error
	StopService(timeout time.Duration) error
	Spawn(exe string, args ...string) error // detached, no window
	PortInUse(addr string) bool
}

// Options for Install. NoSelfUpdate and Quiet are consumed by later tasks.
type Options struct {
	Mode                Mode
	Quiet, NoSelfUpdate bool
	Self, Version       string
	Env                 Env
}

// Quote quotes one command-line argument (paths only: no embedded quotes).
// Trailing backslashes are doubled so they do not escape the closing quote.
func Quote(path string) string {
	n := len(path) - len(strings.TrimRight(path, `\`))
	return `"` + path + strings.Repeat(`\`, n) + `"`
}

// Install copies the binaries into the mode's program dir, registers autostart
// and the uninstall entry, writes the Start-menu link and starts the agent.
// Running it again over an existing install updates it in place.
func Install(sys System, o Options) (Layout, error) {
	l := LayoutFor(o.Mode, o.Env)
	if o.Mode == Service {
		return l, errors.New("service install: not implemented yet")
	}
	selfW := filepath.Join(filepath.Dir(o.Self), exeWName)
	if _, err := os.Stat(selfW); err != nil {
		return l, fmt.Errorf("%s not found next to %s: %w", exeWName, o.Self, err)
	}
	if err := os.MkdirAll(l.ProgramDir, 0o755); err != nil {
		return l, err
	}
	if err := copyExe(o.Self, l.Exe); err != nil {
		return l, err
	}
	if err := copyExe(selfW, l.ExeW); err != nil {
		return l, err
	}
	if err := os.MkdirAll(filepath.Dir(l.StartMenuLink), 0o755); err != nil {
		return l, err
	}
	if err := os.WriteFile(l.StartMenuLink, []byte("[InternetShortcut]\r\nURL="+uiURL+"\r\n"), 0o644); err != nil {
		return l, err
	}
	if err := sys.SetRun(name, Quote(l.ExeW)+" serve"); err != nil {
		return l, err
	}
	err := sys.WriteUninstall(o.Mode, Entry{
		DisplayName:     name,
		DisplayVersion:  o.Version,
		Publisher:       name,
		InstallLocation: l.ProgramDir,
		UninstallString: Quote(l.Exe) + " uninstall",
	})
	if err != nil {
		return l, err
	}
	return l, sys.Spawn(l.ExeW, "serve")
}

// copyExe copies src to dst unless they are the same file. A running dst cannot
// be overwritten on Windows but can be renamed, so it is moved aside first.
func copyExe(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	old := dst + ".old"
	os.Remove(old)
	if os.Rename(dst, old) == nil {
		defer os.Remove(old) // fails silently while the old exe still runs
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Uninstall removes what Install created. The config directory is kept unless purge.
func Uninstall(sys System, m Mode, purge bool, e Env) error {
	l := LayoutFor(m, e)
	if m == Service {
		if ok, _ := sys.ServiceExists(); ok {
			_ = sys.StopService(30 * time.Second)
			if err := sys.RemoveService(); err != nil {
				return err
			}
		}
	} else if err := sys.DeleteRun(name); err != nil {
		return err
	}
	if err := sys.DeleteUninstall(m); err != nil {
		return err
	}
	if err := os.Remove(l.StartMenuLink); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := removeDir(l.ProgramDir); err != nil {
		return err
	}
	if purge {
		return os.RemoveAll(filepath.Dir(l.Config))
	}
	return nil
}

// removeDir deletes dir. A running exe cannot be deleted on Windows but can be
// renamed, so on failure each file is moved to %TEMP%\pdb-print-*.old first
// (MOVEFILE_DELAY_UNTIL_REBOOT would need admin). The leftover is cleaned by the OS temp cleanup.
func removeDir(dir string) error {
	if err := os.RemoveAll(dir); err == nil || os.IsNotExist(err) {
		return nil
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		p := filepath.Join(dir, f.Name())
		if os.Remove(p) == nil {
			continue
		}
		tmp, err := os.CreateTemp("", name+"-*.old")
		if err != nil {
			return err
		}
		tmp.Close()
		os.Remove(tmp.Name())
		if err := os.Rename(p, tmp.Name()); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// Installed reports which mode is installed (per-user first), with the program
// dir taken from its uninstall entry.
func Installed(sys System) (Layout, bool) {
	for _, m := range []Mode{PerUser, Service} {
		if en, ok, err := sys.ReadUninstall(m); err == nil && ok {
			l := LayoutFor(m, EnvFromOS())
			l.ProgramDir = en.InstallLocation
			l.Exe = filepath.Join(l.ProgramDir, exeName)
			l.ExeW = filepath.Join(l.ProgramDir, exeWName)
			return l, true
		}
	}
	return Layout{}, false
}

// portInUse reports whether a TCP listen on addr fails (something already listens).
func portInUse(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	ln.Close()
	return false
}
