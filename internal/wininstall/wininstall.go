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

	"github.com/francisrath/pdb-print/internal/config"
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
type Entry struct {
	DisplayName, DisplayVersion, Publisher, InstallLocation, UninstallString, DisplayIcon string
}

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
	Spawn(exe string, args ...string) error // detached, no window, cwd = exe's dir
	// PortInUse reports whether addr can't be bound: use it to see that a port is free.
	PortInUse(addr string) bool
	// Listening reports whether something accepts connections on addr (a dial, never a bind,
	// so it can't take the port from an agent that is starting).
	Listening(addr string) bool
	// DeleteAfterExit deletes path once this process has exited (a busy exe), in the background.
	DeleteAfterExit(path string) error
	// WaitExit waits until process pid has exited; one that doesn't exist has.
	WaitExit(pid int, timeout time.Duration) error
}

// Options for Install. NoSelfUpdate writes self_update: false into the config; Quiet is for the caller.
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

// ErrNotStarted marks an Install that completed but could not start the agent.
var ErrNotStarted = errors.New("installed, but could not start the agent")

// Install copies the binaries into the mode's program dir, registers autostart
// and the uninstall entry, writes the Start-menu link and starts the agent.
// Running it again over an existing install updates it in place.
func Install(sys System, o Options) (Layout, error) {
	l := LayoutFor(o.Mode, o.Env)
	if err := o.Env.check(o.Mode); err != nil {
		return l, err
	}
	if o.Mode == Service {
		return l, errors.New("service install: not implemented yet")
	}
	selfW := filepath.Join(filepath.Dir(o.Self), exeWName)
	if _, err := os.Stat(selfW); err != nil {
		return l, fmt.Errorf("%s not found next to %s (extract the whole zip first, then run %s from the extracted folder): %w",
			exeWName, o.Self, exeName, err)
	}
	if err := os.MkdirAll(l.ProgramDir, 0o755); err != nil {
		return l, fmt.Errorf("create program dir: %w", err)
	}
	cleanupOld(l.ProgramDir)
	if err := copyExe(o.Self, l.Exe); err != nil {
		return l, fmt.Errorf("copy %s: %w", exeName, err)
	}
	// A failure here leaves a partial update (no rollback of the first copy); the next install redoes both.
	if err := copyExe(selfW, l.ExeW); err != nil {
		return l, fmt.Errorf("copy %s: %w", exeWName, err)
	}
	err := sys.WriteUninstall(o.Mode, Entry{
		DisplayName:     name,
		DisplayVersion:  o.Version,
		Publisher:       name,
		InstallLocation: l.ProgramDir,
		UninstallString: Quote(l.Exe) + " uninstall",
		DisplayIcon:     l.Exe,
	})
	if err != nil {
		return l, fmt.Errorf("write uninstall entry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.StartMenuLink), 0o755); err != nil {
		return l, fmt.Errorf("create Start menu dir: %w", err)
	}
	if err := os.WriteFile(l.StartMenuLink, []byte("[InternetShortcut]\r\nURL="+uiURL+"\r\n"), 0o644); err != nil {
		return l, fmt.Errorf("write Start menu link: %w", err)
	}
	if o.NoSelfUpdate { // before the agent starts, so it reads the new value
		if err := disableSelfUpdate(l.Config); err != nil {
			return l, fmt.Errorf("write config: %w", err)
		}
	}
	if err := sys.SetRun(name, Quote(l.ExeW)+" serve"); err != nil {
		return l, fmt.Errorf("set Run value: %w", err)
	}
	if err := sys.Spawn(l.ExeW, "serve"); err != nil {
		return l, fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	return l, nil
}

func disableSelfUpdate(path string) error {
	cfg, err := config.Load(path, false)
	if err != nil {
		return err
	}
	off := false
	cfg.SelfUpdate = &off
	return cfg.Save(path)
}

// check rejects an Env that lacks the folders mode m needs.
func (e Env) check(m Mode) error {
	type kv struct{ k, v string }
	need := []kv{{"LOCALAPPDATA", e.LocalAppData}, {"APPDATA", e.AppData}, {"Start menu", e.StartMenuUser}}
	if m == Service {
		need = []kv{{"ProgramFiles", e.ProgramFiles}, {"ProgramData", e.ProgramData}, {"Start menu", e.StartMenuAll}}
	}
	for _, n := range need {
		if n.v == "" {
			return fmt.Errorf("cannot locate %s folder (empty)", n.k)
		}
	}
	return nil
}

// copyExe copies src to dst unless they are the same file. It writes dst.new
// first, then swaps it in: a running dst cannot be overwritten on Windows but
// can be renamed, so the old file is moved to dst.old. A failed copy leaves dst untouched.
func copyExe(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	tmp, old := dst+".new", dst+".old"
	if err := writeFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(old)
	moved := os.Rename(dst, old) == nil
	if err := os.Rename(tmp, dst); err != nil {
		if moved {
			os.Rename(old, dst)
		}
		os.Remove(tmp)
		return err
	}
	os.Remove(old) // fails silently while the old exe still runs; cleanupOld retries
	return nil
}

func writeFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
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

// cleanupOld removes leftovers of earlier updates and uninstalls (best effort).
func cleanupOld(dir string) {
	for _, pat := range []string{filepath.Join(dir, "*.old"), filepath.Join(dir, "*.new"), filepath.Join(filepath.Dir(dir), "*."+name+".old.*")} {
		old, _ := filepath.Glob(pat)
		for _, p := range old {
			os.Remove(p)
		}
	}
}

// Uninstall removes what Install created. The config directory is kept unless purge.
// The uninstall entry goes last so a failed uninstall can be retried.
func Uninstall(sys System, m Mode, purge bool, e Env) error {
	if err := e.check(m); err != nil {
		return err
	}
	l := LayoutFor(m, e)
	// read first: if removing the files fails later, this entry is written back (see below), and
	// if it can't be read, nothing has been changed yet
	entry, hadEntry, err := sys.ReadUninstall(m)
	if err != nil {
		return fmt.Errorf("read uninstall entry: %w", err)
	}
	if m == Service {
		ok, err := sys.ServiceExists()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		if ok {
			if err := sys.StopService(30 * time.Second); err != nil {
				return fmt.Errorf("stop service: %w", err)
			}
			if err := sys.RemoveService(); err != nil {
				return fmt.Errorf("remove service: %w", err)
			}
		}
	} else if err := sys.DeleteRun(name); err != nil {
		return fmt.Errorf("delete Run value: %w", err)
	}
	if err := os.Remove(l.StartMenuLink); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Start menu link: %w", err)
	}
	if purge {
		if err := os.RemoveAll(filepath.Dir(l.Config)); err != nil {
			return fmt.Errorf("remove config: %w", err)
		}
	}
	// The uninstall entry runs the exe in the program dir, so it goes before the files do: an entry
	// must never point at a removed exe. If removing the files fails and the exe is still there, the
	// entry comes back, so Settings → Apps can run the uninstall again.
	if err := sys.DeleteUninstall(m); err != nil {
		return fmt.Errorf("delete uninstall entry: %w", err)
	}
	// a process can't remove its own working dir on Windows (Settings may start us in it)
	if wd, err := os.Getwd(); err == nil && within(wd, l.ProgramDir) {
		_ = os.Chdir(filepath.Dir(l.ProgramDir))
	}
	aside, err := removeDirFn(l.ProgramDir)
	var leftover []string // files moved aside that nothing will delete
	for _, p := range aside {
		if sys.DeleteAfterExit(p) != nil {
			leftover = append(leftover, p)
		}
	}
	if err != nil {
		if _, statErr := os.Stat(l.Exe); hadEntry && statErr == nil {
			if werr := sys.WriteUninstall(m, entry); werr != nil {
				return fmt.Errorf("remove program dir: %w; and the uninstall entry couldn't be restored (%v): delete %s by hand%s", err, werr, l.ProgramDir, byHand(leftover))
			}
			return fmt.Errorf("remove program dir (run the uninstall again%s): %w", byHand(leftover), err)
		}
		return fmt.Errorf("remove program dir (delete %s by hand%s): %w", l.ProgramDir, byHand(leftover), err)
	}
	if len(leftover) > 0 {
		return fmt.Errorf("pdb-print is uninstalled, but %s could not be scheduled for deletion: delete it by hand", strings.Join(leftover, ", "))
	}
	return nil
}

// byHand names files the user has to delete, for an error message ("" when there are none).
func byHand(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return "; also delete " + strings.Join(files, ", ")
}

// removeDirFn is removeDir; tests replace it to get files moved aside, which a non-Windows
// system can't produce (it can delete a file in use).
var removeDirFn = removeDir

// removeDir deletes dir. A running exe cannot be deleted on Windows but can be
// renamed, so on failure each busy file is moved next to dir (same volume, unlike
// %TEMP%) as <file>.pdb-print.old.<n>, and returned so they can be deleted once free.
func removeDir(dir string) (aside []string, err error) {
	if err := os.RemoveAll(dir); err == nil || os.IsNotExist(err) {
		return nil, nil
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		p := filepath.Join(dir, f.Name())
		if os.Remove(p) == nil {
			continue
		}
		a := filepath.Join(filepath.Dir(dir), fmt.Sprintf("%s.%s.old.%d", f.Name(), name, time.Now().UnixNano()))
		if err := os.Rename(p, a); err != nil {
			return aside, err
		}
		aside = append(aside, a)
	}
	return aside, os.RemoveAll(dir)
}

// within reports whether path is dir or inside it, ignoring case as Windows does.
func within(path, dir string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DeleteLaterCmdLine is the cmd.exe command line that deletes path once it is no longer busy:
// it retries every second for up to 10 minutes, as the uninstall window may wait for Enter first.
func DeleteLaterCmdLine(path string) string {
	q := `"` + path + `"`
	return `cmd.exe /d /c for /l %i in (1,1,600) do @(ping -n 2 127.0.0.1 >nul & del /f /q ` + q +
		` >nul 2>&1 & if not exist ` + q + ` exit)`
}

// Installed reports which mode is installed (per-user first), with the program
// dir taken from its uninstall entry.
func Installed(sys System, e Env) (Layout, bool, error) {
	for _, m := range []Mode{PerUser, Service} {
		en, ok, err := sys.ReadUninstall(m)
		if err != nil { // can't tell: don't report "not installed"
			return Layout{}, false, fmt.Errorf("read uninstall entry: %w", err)
		}
		if ok && en.InstallLocation != "" {
			l := LayoutFor(m, e)
			l.ProgramDir = en.InstallLocation
			l.Exe = filepath.Join(l.ProgramDir, exeName)
			l.ExeW = filepath.Join(l.ProgramDir, exeWName)
			return l, true, nil
		}
	}
	return Layout{}, false, nil
}

// listening reports whether something accepts TCP connections on addr.
func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// portInUse reports whether addr is already bound. Other listen errors
// (malformed address, reserved port) are not "in use".
func portInUse(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return isAddrInUse(err)
	}
	ln.Close()
	return false
}
