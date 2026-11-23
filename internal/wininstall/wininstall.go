// Package wininstall installs pdb-print on Windows: program files, autostart,
// Start-menu link, "Apps & features" entry and (later) the service. All
// registry/service/process access goes through System so the logic is testable
// anywhere.
package wininstall

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
)

const (
	name      = "pdb-print"
	exeName   = "pdb-print.exe"
	exeWName  = "pdb-printw.exe"
	agentAddr = "127.0.0.1:8631"
	uiURL     = "http://" + agentAddr
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
	ServiceStopped() (bool, error) // the service's state is Stopped
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
	// OwnedByAdmins reports whether path is owned by Administrators or SYSTEM.
	OwnedByAdmins(path string) (bool, error)
}

// Options for Install. NoSelfUpdate writes self_update: false into the config; Quiet is for the caller.
// Out gets notices for the user (nil = none).
type Options struct {
	Mode                Mode
	Quiet, NoSelfUpdate bool
	Self, Version       string
	Env                 Env
	Out                 io.Writer
}

// ErrCantWatch is WaitExit's answer for a process it may not open (another user's, or an elevated
// one): wait for its port to free instead.
var ErrCantWatch = errors.New("can't watch the process")

// serviceStopWait is how long a stopping service may take: serve's own shutdown (a print in
// progress finishes, up to 2 min) and a little more.
const serviceStopWait = 2*time.Minute + 10*time.Second

// Quote quotes one command-line argument (paths only: no embedded quotes).
// Trailing backslashes are doubled so they do not escape the closing quote.
func Quote(path string) string {
	n := len(path) - len(strings.TrimRight(path, `\`))
	return `"` + path + strings.Repeat(`\`, n) + `"`
}

// ErrNotStarted marks an Install that completed but could not start the agent.
var ErrNotStarted = errors.New("installed, but could not start the agent")

// ErrPerUserStarted marks a service Install that failed after which the per-user agent it was to
// replace was started again.
var ErrPerUserStarted = errors.New("the per-user pdb-print was started again")

// Install copies the binaries into the mode's program dir, registers autostart
// and the uninstall entry, writes the Start-menu link and starts the agent.
// Running it again over an existing install updates it in place.
func Install(sys System, o Options) (l Layout, err error) {
	l = LayoutFor(o.Mode, o.Env)
	if err := o.Env.check(o.Mode); err != nil {
		return l, err
	}
	selfW := filepath.Join(filepath.Dir(o.Self), exeWName)
	if _, err := os.Stat(selfW); err != nil {
		return l, fmt.Errorf("%s not found next to %s (extract the whole zip first, then run %s from the extracted folder): %w",
			exeWName, o.Self, exeName, err)
	}
	if o.Mode == Service {
		hadService, qerr := sys.ServiceExists() // not err: the defer below must see the result
		if qerr != nil {
			return l, fmt.Errorf("query service: %w", qerr)
		}
		if hadService { // waits for a print in progress; its exes can then be swapped
			if err := sys.StopService(serviceStopWait); err != nil {
				return l, fmt.Errorf("stop service: %w", err)
			}
			defer func() {
				if err != nil && !errors.Is(err, ErrNotStarted) {
					err = fmt.Errorf("%w (the pdb-print service is stopped; run the install again)", err)
				}
			}()
		}
		if sys.PortInUse(agentAddr) { // not our (now stopped) service, nor the per-user agent the caller stopped
			return l, errors.New("port 8631 is in use by another program: stop it and run the install again")
		}
		// before any file goes in it: ProgramData lets every user create files (a planted session.key)
		if err := readyConfigDir(sys, filepath.Dir(l.Config), hadService, o.Out); err != nil {
			return l, err
		}
		// every install (an upgrade too) signs every browser out: a cookie may have reached another
		// program on 127.0.0.1 (cookies ignore ports)
		if err := newSessionKey(filepath.Join(filepath.Dir(l.Config), "session.key")); err != nil {
			return l, fmt.Errorf("write session key: %w", err)
		}
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
	err = sys.WriteUninstall(o.Mode, Entry{
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
	var perUser Layout // the per-user install the service replaces (ProgramDir "" = none)
	copied := false    // its config is the service's now
	if o.Mode == Service {
		if perUser, copied, err = copyPerUserConfig(sys, o, l.Config); err != nil {
			return l, err
		}
		if _, err := os.Stat(l.Config); errors.Is(err, os.ErrNotExist) { // service run --config needs one
			if err := WriteAdminOnly(l.Config, nil); err != nil {
				return l, fmt.Errorf("write config: %w", err)
			}
		}
	}
	if o.NoSelfUpdate { // before the agent starts, so it reads the new value
		if err := disableSelfUpdate(l.Config, o.Mode); err != nil {
			return l, fmt.Errorf("write config: %w", err)
		}
	}
	if o.Mode == Service {
		// creates it, or refreshes an existing one's config; raw paths: the service manager quotes them
		if err := sys.InstallService(l.Exe, []string{"service", "run", "--config", l.Config}); err != nil {
			return l, fmt.Errorf("install service: %w", err)
		}
		// Running only means the handler is up: serve may still fail on its config or the port
		err := sys.StartService()
		if err == nil {
			err = waitServing(sys)
		}
		if err != nil {
			err = fmt.Errorf("%w: the pdb-print service didn't start (%v); see %s, or Event Viewer → Windows Logs → Application (source pdb-print) if there is none",
				ErrNotStarted, err, filepath.Join(filepath.Dir(l.Config), "pdb-print.log"))
			if perUser.ProgramDir != "" { // the caller stopped it; it is still installed
				if serr := sys.Spawn(perUser.ExeW, "serve"); serr != nil {
					return l, fmt.Errorf("%w; and the per-user pdb-print couldn't be started again: %v", err, serr)
				}
				return l, fmt.Errorf("%w; %w", err, ErrPerUserStarted)
			}
			return l, err
		}
		if perUser.ProgramDir != "" {
			return l, removePerUser(sys, o, perUser, copied && keychainTokens(l.Config))
		}
		return l, nil
	}
	if err := sys.SetRun(name, Quote(l.ExeW)+" serve"); err != nil {
		return l, fmt.Errorf("set Run value: %w", err)
	}
	if err := sys.Spawn(l.ExeW, "serve"); err != nil {
		return l, fmt.Errorf("%w: %v", ErrNotStarted, err)
	}
	return l, nil
}

// Swapped by tests.
var (
	serveWait = 15 * time.Second
	servePoll = 250 * time.Millisecond
)

// waitServing waits until the started service listens on agentAddr; a service that stops fails at once.
func waitServing(sys System) error {
	for end := time.Now().Add(serveWait); !sys.Listening(agentAddr); time.Sleep(servePoll) {
		if stopped, err := sys.ServiceStopped(); err != nil {
			return err
		} else if stopped {
			return errors.New("it stopped")
		}
		if time.Now().After(end) {
			return fmt.Errorf("not listening on %s after %s", agentAddr, serveWait)
		}
	}
	// something listens: it isn't ours if the service has died meanwhile
	if stopped, err := sys.ServiceStopped(); err != nil {
		return err
	} else if stopped {
		return fmt.Errorf("it stopped, and something else listens on %s", agentAddr)
	}
	return nil
}

// cleanConfigDir readies a trusted config dir for a first install: an old pairing code goes (someone
// may know it). The session key is new on every install (newSessionKey).
func cleanConfigDir(dir string) error {
	if err := os.Remove(filepath.Join(dir, "pair-code.txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// untrusted says why dir can't be trusted as the service's config dir ("" = it can): it, or anything
// in it, is a link, or is not owned by Administrators or SYSTEM. This is a check, not a lock: a user
// who can still write somewhere in the tree could swap a file in after it. That fails closed: the
// service then refuses its folder (or ignores the file), so the most such a user gets is a service
// that doesn't start, never their file trusted.
func untrusted(sys System, dir string) (string, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() && !isReparse(fi) {
		return "it is not a folder", nil
	}
	why := ""
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if isReparse(fi) { // before the owner: that check follows links
			why = p + " is a link"
			return filepath.SkipAll
		}
		owned, err := sys.OwnedByAdmins(p)
		if err != nil {
			return fmt.Errorf("owner of %s: %w", p, err)
		} else if !owned {
			why = p + " was not made by an administrator"
			return filepath.SkipAll
		}
		return nil
	})
	return why, err
}

// readyConfigDir makes dir the service's admin-only config dir. One that isn't trusted (untrusted) is
// moved aside rather than re-ACL'd in place (its maker may hold a handle to it and swap it later),
// and a new one is created with the ACL. A first install also cleans a trusted one (cleanConfigDir).
func readyConfigDir(sys System, dir string, hadService bool, out io.Writer) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return createConfigDir(dir)
	}
	why, err := untrusted(sys, dir)
	if err != nil {
		return err
	}
	if why != "" {
		aside := fmt.Sprintf("%s.untrusted.%d", dir, time.Now().UnixNano())
		if _, err := os.Lstat(aside); err == nil {
			return fmt.Errorf("can't move %s aside: %s exists already; run the install again", dir, aside)
		}
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("can't move %s aside to %s (%s): %w; if another user has it open, sign them out or restart Windows, then run the install again", dir, aside, why, err)
		}
		if out != nil {
			fmt.Fprintf(out, "Moved %s aside to %s: %s.\n", dir, aside, why)
		}
		return createConfigDir(dir)
	}
	if err := MkdirAdminOnly(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if !hadService {
		return cleanConfigDir(dir)
	}
	return nil
}

// ErrUntrustedDir: ReadyServiceDir refused the config dir (see untrusted).
var ErrUntrustedDir = errors.New("not starting: untrusted config folder")

// ReadyServiceDir is what the service does before it opens its log: the config dir must be trusted
// (see untrusted), else it doesn't start; a missing one (deleted by an admin) is created admin-only.
// A missing config.yaml is written empty. A trusted dir gets the admin-only ACL again (an admin may
// have recreated it with ProgramData's, readable by every user); an untrusted one is never re-ACL'd.
func ReadyServiceDir(sys System, cfgPath string) error {
	dir := filepath.Dir(cfgPath)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err := createConfigDir(dir); err != nil {
			return err
		}
	} else if why, err := untrusted(sys, dir); err != nil {
		return err
	} else if why != "" {
		return fmt.Errorf("%w %s (%s): delete it and run pdb-print install --service again", ErrUntrustedDir, dir, why)
	} else if err := MkdirAdminOnly(dir); err != nil { // the whole tree is admins': safe in place
		return fmt.Errorf("reset %s: %w", dir, err) // e.g. an admin recreated it with the inherited ACL
	}
	if _, err := os.Lstat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return WriteAdminOnly(cfgPath, nil)
	}
	return nil
}

// createConfigDir creates dir admin-only; one that exists already was made by someone else meanwhile.
func createConfigDir(dir string) error {
	err := createAdminOnly(dir)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s was created by someone else while the install ran: run the install again", dir)
	} else if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// notLink refuses a path that is a link (symlink, junction): the admin-only ACL would land elsewhere.
func notLink(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && isReparse(fi) {
		return fmt.Errorf("%s is a link (junction or symlink), not a folder: delete it and run the install again", dir)
	}
	return nil
}

// copyPerUserConfig finds this user's per-user install, which the service replaces, and copies its
// config to cfg (unless the service has one already; copied says whether it was). The caller has
// stopped the per-user agent; it is removed (removePerUser) only once the service serves.
func copyPerUserConfig(sys System, o Options, cfg string) (pu Layout, copied bool, err error) {
	pu, ok, err := InstalledAs(sys, PerUser, o.Env)
	if err != nil {
		return Layout{}, false, fmt.Errorf("per-user install: %w", err)
	} else if !ok {
		return Layout{}, false, nil
	}
	if _, err := os.Stat(cfg); errors.Is(err, os.ErrNotExist) {
		b, err := os.ReadFile(pu.Config)
		if err == nil {
			if err := WriteAdminOnly(cfg, b); err != nil {
				return Layout{}, false, fmt.Errorf("copy config: %w", err)
			}
			copied = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return Layout{}, false, fmt.Errorf("read per-user config: %w", err)
		}
	}
	return pu, copied, nil
}

// removePerUser removes the per-user install pu (its config stays) once the service serves;
// keychain: the copied config has a token in that user's keychain.
func removePerUser(sys System, o Options, pu Layout, keychain bool) error {
	if err := Uninstall(sys, PerUser, false, o.Env); err != nil {
		return fmt.Errorf("the service runs, but the per-user install couldn't be removed (remove it in Settings → Apps): %w", err)
	}
	if o.Out == nil {
		return nil
	}
	fmt.Fprintf(o.Out, "Replaced the per-user install of pdb-print; its config stays in %s.\n", filepath.Dir(pu.Config))
	if keychain {
		// the token is in this user's keychain; the service (LocalSystem) has its own
		fmt.Fprintln(o.Out, "The service can't read this user's keychain: re-enter the Part-DB token in the web UI (pair with `pdb-print pair`).")
	}
	return nil
}

// keychainTokens reports whether the config has an instance whose token isn't in the file.
func keychainTokens(path string) bool {
	cfg, err := config.Load(path, true)
	if err != nil {
		return false
	}
	for _, in := range cfg.Instances {
		if in.Token == "" {
			return true
		}
	}
	return false
}

func disableSelfUpdate(path string, m Mode) error {
	cfg, err := config.Load(path, false)
	if err != nil {
		return err
	}
	off := false
	cfg.SelfUpdate = &off
	if err := cfg.Save(path); err != nil || m != Service {
		return err
	}
	b, err := os.ReadFile(path) // again, owned by Administrators (Save's file is this account's)
	if err != nil {
		return err
	}
	return WriteAdminOnly(path, b)
}

// newSessionKey writes a new random key for the service's browser sessions (see server.sessionKey).
func newSessionKey(path string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	return WriteAdminOnly(path, key)
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
			if err := sys.StopService(serviceStopWait); err != nil {
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
	LeaveDir(l.ProgramDir) // Settings may start us in it
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

// LeaveDir moves this process's working dir out of dir if it is in it: on Windows a dir that is
// some process's working dir can't be removed.
func LeaveDir(dir string) {
	if wd, err := os.Getwd(); err == nil && within(wd, dir) {
		_ = os.Chdir(filepath.Dir(dir))
	}
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
		if l, ok, err := InstalledAs(sys, m, e); err != nil || ok {
			return l, ok, err
		}
	}
	return Layout{}, false, nil
}

// InstalledAs reports whether mode m is installed, with the program dir taken from its uninstall entry.
func InstalledAs(sys System, m Mode, e Env) (Layout, bool, error) {
	en, ok, err := sys.ReadUninstall(m)
	if err != nil { // can't tell: don't report "not installed"
		return Layout{}, false, fmt.Errorf("read uninstall entry: %w", err)
	}
	if !ok || en.InstallLocation == "" {
		return Layout{}, false, nil
	}
	l := LayoutFor(m, e)
	l.ProgramDir = en.InstallLocation
	l.Exe = filepath.Join(l.ProgramDir, exeName)
	l.ExeW = filepath.Join(l.ProgramDir, exeWName)
	return l, true, nil
}

// ModeOf reports which mode's program dir exe is in (false: neither, e.g. a copy run from a zip).
// Short (8.3) names are expanded first.
func ModeOf(exe string, e Env) (Mode, bool) {
	exe = longPath(exe)
	for _, m := range []Mode{Service, PerUser} {
		if d := LayoutFor(m, e).ProgramDir; filepath.IsAbs(d) && within(exe, longPath(d)) {
			return m, true
		}
	}
	return PerUser, false
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
