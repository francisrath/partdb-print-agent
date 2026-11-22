package wininstall

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
)

type fakeSystem struct {
	run      map[string]string
	unin     map[Mode]Entry
	service  bool
	spawns   [][]string
	uninErr  error           // DeleteUninstall fails with it
	readErr  error           // ReadUninstall fails with it
	delErr   error           // DeleteAfterExit fails with it
	writeOK  int             // WriteUninstall calls that succeed before it fails (-1 = always)
	calls    []string        // service calls in order
	startErr error           // StartService fails with it
	notOwned map[string]bool // base names OwnedByAdmins says an ordinary user owns
	busy     bool            // PortInUse says yes
}

func newFake() *fakeSystem {
	return &fakeSystem{run: map[string]string{}, unin: map[Mode]Entry{}, writeOK: -1}
}

func (f *fakeSystem) IsAdmin() bool            { return true }
func (f *fakeSystem) SetRun(n, c string) error { f.run[n] = c; return nil }
func (f *fakeSystem) DeleteRun(n string) error { delete(f.run, n); return nil }
func (f *fakeSystem) WriteUninstall(m Mode, e Entry) error {
	if f.writeOK == 0 {
		return errors.New("registry full")
	}
	f.writeOK--
	f.unin[m] = e
	return nil
}
func (f *fakeSystem) DeleteUninstall(m Mode) error {
	if f.uninErr != nil {
		return f.uninErr
	}
	delete(f.unin, m)
	return nil
}
func (f *fakeSystem) ServiceExists() (bool, error) { return f.service, nil }
func (f *fakeSystem) InstallService(exe string, args []string) error {
	f.calls = append(f.calls, "install "+strings.Join(append([]string{exe}, args...), "|"))
	f.service = true
	return nil
}
func (f *fakeSystem) RemoveService() error {
	f.calls = append(f.calls, "remove")
	f.service = false
	return nil
}
func (f *fakeSystem) StartService() error { f.calls = append(f.calls, "start"); return f.startErr }
func (f *fakeSystem) StopService(d time.Duration) error {
	f.calls = append(f.calls, "stop "+d.String())
	return nil
}
func (f *fakeSystem) PortInUse(string) bool             { return f.busy }
func (f *fakeSystem) Listening(string) bool             { return false }
func (f *fakeSystem) DeleteAfterExit(string) error      { return f.delErr }
func (f *fakeSystem) WaitExit(int, time.Duration) error { return nil }
func (f *fakeSystem) OwnedByAdmins(p string) (bool, error) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 { // the real one follows links
		return false, errors.New("follows the link")
	}
	return !f.notOwned[filepath.Base(p)], nil
}
func (f *fakeSystem) Spawn(exe string, args ...string) error {
	f.spawns = append(f.spawns, append([]string{exe}, args...))
	return nil
}
func (f *fakeSystem) ReadUninstall(m Mode) (Entry, bool, error) {
	if f.readErr != nil {
		return Entry{}, false, f.readErr
	}
	e, ok := f.unin[m]
	return e, ok, nil
}

func testEnv(t *testing.T) Env {
	t.Helper()
	r := filepath.Join(t.TempDir(), "Jöhn Doe")
	return Env{
		LocalAppData:  filepath.Join(r, "Local"),
		AppData:       filepath.Join(r, "Roaming"),
		ProgramFiles:  filepath.Join(r, "PF"),
		ProgramData:   filepath.Join(r, "PD"),
		StartMenuUser: filepath.Join(r, "SMU"),
		StartMenuAll:  filepath.Join(r, "SMA"),
	}
}

// fakeSelf creates src/pdb-print.exe and src/pdb-printw.exe.
func fakeSelf(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pdb-print.exe", "pdb-printw.exe"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("bin "+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "pdb-print.exe")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestLayoutFor(t *testing.T) {
	e := testEnv(t)
	tests := []struct {
		m                 Mode
		progDir, config   string
		startMenuLinkBase string
	}{
		{PerUser, filepath.Join(e.LocalAppData, "Programs", "pdb-print"), filepath.Join(e.AppData, "pdb-print", "config.yaml"), e.StartMenuUser},
		{Service, filepath.Join(e.ProgramFiles, "pdb-print"), filepath.Join(e.ProgramData, "pdb-print", "config.yaml"), e.StartMenuAll},
	}
	for _, tt := range tests {
		l := LayoutFor(tt.m, e)
		if l.Mode != tt.m || l.ProgramDir != tt.progDir || l.Config != tt.config {
			t.Errorf("mode %d: got %+v", tt.m, l)
		}
		if l.Exe != filepath.Join(tt.progDir, "pdb-print.exe") || l.ExeW != filepath.Join(tt.progDir, "pdb-printw.exe") {
			t.Errorf("mode %d exes: %+v", tt.m, l)
		}
		if l.StartMenuLink != filepath.Join(tt.startMenuLinkBase, "pdb-print.url") {
			t.Errorf("mode %d link: %q", tt.m, l.StartMenuLink)
		}
	}
}

func TestInstallPerUser(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	o := Options{Mode: PerUser, Self: fakeSelf(t), Version: "1.2.3", Env: e}
	l, err := Install(sys, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{l.Exe, l.ExeW} {
		if !exists(p) {
			t.Errorf("%s not copied", p)
		}
	}
	if got, want := sys.run["pdb-print"], Quote(l.ExeW)+" serve"; got != want {
		t.Errorf("run = %q, want %q", got, want)
	}
	en := sys.unin[PerUser]
	if en.DisplayVersion != "1.2.3" || en.UninstallString != Quote(l.Exe)+" uninstall" || en.InstallLocation != l.ProgramDir || en.DisplayIcon != l.Exe {
		t.Errorf("entry = %+v", en)
	}
	b, err := os.ReadFile(l.StartMenuLink)
	if err != nil || !strings.Contains(string(b), "URL=http://127.0.0.1:8631") {
		t.Errorf("link = %q, %v", b, err)
	}
	if len(sys.spawns) != 1 || sys.spawns[0][0] != l.ExeW || sys.spawns[0][1] != "serve" {
		t.Errorf("spawns = %v", sys.spawns)
	}
}

func TestInstallTwiceIsInPlace(t *testing.T) {
	sys, o := newFake(), Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: testEnv(t)}
	l, err := Install(sys, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.Self, []byte("new build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(sys, o); err != nil {
		t.Fatal(err)
	}
	if len(sys.run) != 1 {
		t.Errorf("run values = %v", sys.run)
	}
	if b, _ := os.ReadFile(l.Exe); string(b) != "new build" {
		t.Errorf("exe not replaced: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(l.ProgramDir, "*.old")); len(left) > 0 {
		t.Errorf("leftover %v", left)
	}
	if left, _ := filepath.Glob(filepath.Join(l.ProgramDir, "*.new")); len(left) > 0 {
		t.Errorf("leftover %v", left)
	}
}

func TestCopyExeFailureKeepsOriginal(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "x.exe")
	if err := os.WriteFile(dst, []byte("good"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory as source opens fine but cannot be read, so the copy fails midway.
	if err := copyExe(t.TempDir(), dst); err == nil {
		t.Fatal("want error")
	}
	if b, _ := os.ReadFile(dst); string(b) != "good" {
		t.Errorf("original damaged: %q", b)
	}
	if left, _ := filepath.Glob(dst + ".*"); len(left) > 0 {
		t.Errorf("leftover %v", left)
	}
}

func TestInstallEmptyEnv(t *testing.T) {
	e := testEnv(t)
	e.LocalAppData = ""
	if _, err := Install(newFake(), Options{Mode: PerUser, Self: fakeSelf(t), Env: e}); err == nil {
		t.Error("want error for empty LOCALAPPDATA")
	}
}

type failSpawn struct{ *fakeSystem }

func (failSpawn) Spawn(string, ...string) error { return errors.New("boom") }

func TestInstallSpawnFailure(t *testing.T) {
	sys := failSpawn{newFake()}
	_, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: testEnv(t)})
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("err = %v", err)
	}
	if len(sys.unin) != 1 {
		t.Error("install should be complete")
	}
}

func TestInstallFromProgramDir(t *testing.T) {
	sys, o := newFake(), Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: testEnv(t)}
	l, err := Install(sys, o)
	if err != nil {
		t.Fatal(err)
	}
	o.Self = l.Exe
	if _, err := Install(sys, o); err != nil {
		t.Fatalf("install from program dir: %v", err)
	}
	if b, _ := os.ReadFile(l.Exe); string(b) != "bin pdb-print.exe" {
		t.Errorf("exe content = %q", b)
	}
}

func TestInstallMissingGUIExe(t *testing.T) {
	self := fakeSelf(t)
	os.Remove(filepath.Join(filepath.Dir(self), "pdb-printw.exe"))
	if _, err := Install(newFake(), Options{Mode: PerUser, Self: self, Env: testEnv(t)}); err == nil ||
		!strings.Contains(err.Error(), "pdb-printw.exe not found") || !strings.Contains(err.Error(), "extract the whole zip first") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallServiceTakesOverPerUser(t *testing.T) {
	for _, tt := range []struct {
		name, perUserCfg, serviceCfg, want string
		token                              bool // the re-enter-the-token notice
	}{
		{"keychain token", "instances:\n  - id: home\n    base_url: https://p.example\n", "", "instances:\n  - id: home\n    base_url: https://p.example\n", true},
		{"token in config", "instances:\n  - id: home\n    base_url: https://p.example\n    token: t\n", "", "instances:\n  - id: home\n    base_url: https://p.example\n    token: t\n", false},
		{"service config kept", "instances:\n  - id: home\n    base_url: https://p.example\n", "log_level: debug\n", "log_level: debug\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sys, e := newFake(), testEnv(t)
			pu, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
			if err != nil {
				t.Fatal(err)
			}
			os.MkdirAll(filepath.Dir(pu.Config), 0o755)
			os.WriteFile(pu.Config, []byte(tt.perUserCfg), 0o600)
			ls := LayoutFor(Service, e)
			if tt.serviceCfg != "" {
				os.MkdirAll(filepath.Dir(ls.Config), 0o755)
				os.WriteFile(ls.Config, []byte(tt.serviceCfg), 0o600)
			}
			var out strings.Builder
			l, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Version: "2", Env: e, Out: &out})
			if err != nil {
				t.Fatal(err)
			}
			if l != ls {
				t.Errorf("layout %+v", l)
			}
			for _, p := range []string{l.Exe, l.ExeW, l.StartMenuLink} {
				if !exists(p) {
					t.Errorf("%s missing", p)
				}
			}
			want := []string{"install " + l.Exe + "|service|run|--config|" + l.Config, "start"}
			if strings.Join(sys.calls, ";") != strings.Join(want, ";") {
				t.Errorf("calls %q, want %q", sys.calls, want)
			}
			if en := sys.unin[Service]; en.DisplayVersion != "2" || en.InstallLocation != l.ProgramDir || en.DisplayIcon != l.Exe ||
				en.UninstallString != Quote(l.Exe)+" uninstall" {
				t.Errorf("service entry %+v", en)
			}
			// the per-user install is gone, its config stays
			if _, ok := sys.unin[PerUser]; ok || len(sys.run) != 0 || exists(pu.ProgramDir) || exists(pu.StartMenuLink) || !exists(pu.Config) {
				t.Errorf("per-user leftovers: run %v, entries %v", sys.run, sys.unin)
			}
			if b, _ := os.ReadFile(l.Config); string(b) != tt.want {
				t.Errorf("service config %q, want %q", b, tt.want)
			}
			if fi, err := os.Stat(filepath.Dir(l.Config)); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
				t.Errorf("config dir %v %v", fi.Mode(), err)
			}
			if got := strings.Contains(out.String(), "re-enter the Part-DB token in the web UI (pair with `pdb-print pair`)"); got != tt.token {
				t.Errorf("token notice %v:\n%s", got, out.String())
			}
		})
	}
}

// Over an existing service: stopped first (waiting for a print), the exes swapped, started again.
func TestInstallServiceReinstall(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e}); err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	l, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e, NoSelfUpdate: true})
	if err != nil {
		t.Fatal(err)
	}
	// the service's config is refreshed as on a fresh install
	if got, want := strings.Join(sys.calls, ";"), "stop 2m10s;install "+l.Exe+"|service|run|--config|"+l.Config+";start"; got != want {
		t.Errorf("calls %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(l.Config); !strings.Contains(string(b), "self_update: false") {
		t.Errorf("config %q", b)
	}
	const stopped = "(the pdb-print service is stopped; run the install again)"
	sys.startErr = errors.New("boom")
	if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e}); !errors.Is(err, ErrNotStarted) || strings.Contains(err.Error(), stopped) {
		t.Errorf("start failure: %v", err)
	}
	sys.startErr, sys.writeOK = nil, 0
	if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e}); err == nil || !strings.Contains(err.Error(), stopped) {
		t.Errorf("failure after the stop: %v", err)
	}
}

// A fresh service install keeps only what an administrator (or SYSTEM) made in the config dir, and never
// an old session key or pairing code; a reinstall over the service keeps them.
func TestInstallServiceCleansConfigDir(t *testing.T) {
	for _, tt := range []struct {
		name       string
		notOwned   []string
		hadService bool
		kept, gone []string
	}{
		{"fresh", []string{"planted.txt", "sub"}, false, []string{"config.yaml", "pdb-print.log"}, []string{"session.key", "pair-code.txt", "planted.txt", "sub"}},
		{"planted config", []string{"config.yaml"}, false, []string{"pdb-print.log"}, []string{"session.key"}}, // config: an empty one
		{"reinstall", []string{"planted.txt"}, true, []string{"config.yaml", "session.key", "pair-code.txt", "planted.txt"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sys, e := newFake(), testEnv(t)
			sys.service, sys.notOwned = tt.hadService, map[string]bool{}
			for _, n := range tt.notOwned {
				sys.notOwned[n] = true
			}
			dir := filepath.Dir(LayoutFor(Service, e).Config)
			os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
			for _, n := range []string{"config.yaml", "pdb-print.log", "session.key", "pair-code.txt", "planted.txt"} {
				os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
			}
			var out strings.Builder
			if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e, Out: &out}); err != nil {
				t.Fatal(err)
			}
			for _, n := range tt.kept {
				if !exists(filepath.Join(dir, n)) {
					t.Errorf("%s removed", n)
				}
			}
			for _, n := range tt.gone {
				if exists(filepath.Join(dir, n)) {
					t.Errorf("%s kept", n)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); tt.name == "planted config" && len(b) != 0 {
				t.Errorf("planted config kept: %q", b)
			}
			if !tt.hadService && !strings.Contains(out.String(), "not made by an administrator") {
				t.Errorf("no notice:\n%s", out.String())
			}
		})
	}
}

// A link in the config dir (a dangling one too) is removed without asking for its owner.
func TestInstallServiceRemovesLinks(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	dir := filepath.Dir(LayoutFor(Service, e).Config)
	os.MkdirAll(dir, 0o755)
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, "session.lnk")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	var out strings.Builder
	if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "session.lnk")); err == nil || !strings.Contains(out.String(), "session.lnk: a link") {
		t.Errorf("link kept (%v):\n%s", err, out.String())
	}
}

// A fresh service install with no per-user install to take over still leaves a config for `service run --config`.
func TestInstallServiceWritesEmptyConfig(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(l.Config); err != nil || len(b) != 0 {
		t.Errorf("config %q %v", b, err)
	}
	if _, err := config.Load(l.Config, true); err != nil {
		t.Errorf("empty config doesn't load: %v", err)
	}
}

// A config dir an ordinary user made, a link or a file is moved aside, never re-ACL'd in place; a new
// one is created. One administrators own stays (and is cleaned on a first install).
func TestInstallServiceMovesUntrustedDirAside(t *testing.T) {
	for _, tt := range []struct {
		name, why string
		make      func(t *testing.T, dir string)
	}{
		{"not owned", "not made by an administrator", func(t *testing.T, dir string) {
			os.MkdirAll(dir, 0o777)
			os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("log_level: debug\n"), 0o666)
		}},
		{"link", "is a link", func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Dir(dir), 0o755)
			if err := os.Symlink(t.TempDir(), dir); err != nil {
				t.Skip("no symlinks here:", err)
			}
		}},
		{"file", "not a folder", func(t *testing.T, dir string) {
			os.MkdirAll(filepath.Dir(dir), 0o755)
			os.WriteFile(dir, []byte("x"), 0o666)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sys, e := newFake(), testEnv(t)
			sys.notOwned = map[string]bool{"pdb-print": tt.name == "not owned"}
			dir := filepath.Dir(LayoutFor(Service, e).Config)
			tt.make(t, dir)
			var out strings.Builder
			l, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e, Out: &out})
			if err != nil {
				t.Fatal(err)
			}
			aside, _ := filepath.Glob(dir + ".untrusted.*")
			if len(aside) != 1 || !strings.Contains(out.String(), "Moved "+dir+" aside to "+aside[0]) || !strings.Contains(out.String(), tt.why) {
				t.Errorf("aside %v:\n%s", aside, out.String())
			}
			fi, err := os.Lstat(dir)
			if err != nil || !fi.IsDir() || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
				t.Fatalf("new dir %v %v", fi, err)
			}
			if b, err := os.ReadFile(l.Config); err != nil || len(b) != 0 { // the planted config stays aside
				t.Errorf("config %q %v", b, err)
			}
		})
	}
	t.Run("admin owned", func(t *testing.T) {
		sys, e := newFake(), testEnv(t)
		l := LayoutFor(Service, e)
		os.MkdirAll(filepath.Dir(l.Config), 0o777)
		os.WriteFile(l.Config, []byte("log_level: debug\n"), 0o600)
		var out strings.Builder
		if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e, Out: &out}); err != nil {
			t.Fatal(err)
		}
		if aside, _ := filepath.Glob(filepath.Dir(l.Config) + ".untrusted.*"); len(aside) != 0 || strings.Contains(out.String(), "aside") {
			t.Errorf("moved aside: %v\n%s", aside, out.String())
		}
		if b, _ := os.ReadFile(l.Config); string(b) != "log_level: debug\n" {
			t.Errorf("config %q", b)
		}
	})
}

// Port 8631 held by another program: refused before anything changes; a service that doesn't come up names the log.
func TestInstallServicePortAndStart(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	sys.busy = true
	l := LayoutFor(Service, e)
	if _, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e}); err == nil || !strings.Contains(err.Error(), "port 8631 is in use") {
		t.Errorf("busy port: %v", err)
	}
	if exists(l.ProgramDir) || exists(filepath.Dir(l.Config)) || len(sys.calls) != 0 {
		t.Errorf("changed things: %v", sys.calls)
	}
	sys.busy, sys.startErr = false, errors.New("it stopped")
	_, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e})
	if !errors.Is(err, ErrNotStarted) || !strings.Contains(err.Error(), "didn't start (it stopped); see "+filepath.Join(filepath.Dir(l.Config), "pdb-print.log")) {
		t.Errorf("start failure: %v", err)
	}
}

func TestMkdirAdminOnlyRefusesLink(t *testing.T) {
	r := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(r, "pdb-print")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := MkdirAdminOnly(filepath.Join(r, "pdb-print")); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("err = %v", err)
	}
}

func TestUninstallServiceStopsFirst(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: Service, Self: fakeSelf(t), Env: e})
	if err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	if err := Uninstall(sys, Service, false, e); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sys.calls, ";"); got != "stop 2m10s;remove" {
		t.Errorf("calls %q", got)
	}
	if exists(l.ProgramDir) || exists(l.StartMenuLink) || len(sys.unin) != 0 {
		t.Error("leftovers")
	}
}

func TestMkdirAdminOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pd", "pdb-print")
	if err := os.MkdirAll(dir, 0o777); err != nil { // existing and wide open: reset
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777)
	for range 2 {
		if err := MkdirAdminOnly(dir); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(dir); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
			t.Errorf("mode %v %v", fi.Mode(), err)
		}
		os.RemoveAll(dir) // the second round creates it
	}
}

func TestUninstall(t *testing.T) {
	for _, purge := range []bool{false, true} {
		sys, e := newFake(), testEnv(t)
		l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(l.Config), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.Config, []byte("x: 1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Uninstall(sys, PerUser, purge, e); err != nil {
			t.Fatal(err)
		}
		if exists(l.ProgramDir) || exists(l.StartMenuLink) || len(sys.run) != 0 || len(sys.unin) != 0 {
			t.Errorf("purge=%v: leftovers", purge)
		}
		if got := exists(l.Config); got == purge {
			t.Errorf("purge=%v: config exists=%v", purge, got)
		}
	}
}

// The uninstall entry runs the installed exe, so a failure before the entry is gone must leave
// the exe in place: Settings → Apps can then run the uninstall again.
func TestUninstallEntryNeverPointsAtRemovedExe(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	if err != nil {
		t.Fatal(err)
	}
	sys.uninErr = errors.New("registry says no")
	if err := Uninstall(sys, PerUser, true, e); err == nil {
		t.Fatal("no error")
	}
	if _, ok := sys.unin[PerUser]; !ok || !exists(l.Exe) {
		t.Errorf("entry kept=%v, exe kept=%v; want both", ok, exists(l.Exe))
	}
}

// Removing the files fails while the exe is still there: the entry comes back so Settings → Apps
// can run the uninstall again.
func TestUninstallRestoresEntryWhenFilesStay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory doesn't stop deletion on Windows")
	}
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.ProgramDir, 0o555); err != nil { // its files can't be removed or renamed
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.ProgramDir, 0o755) })
	err = Uninstall(sys, PerUser, false, e)
	if err == nil || !strings.Contains(err.Error(), "run the uninstall again") {
		t.Fatalf("err = %v", err)
	}
	if got, ok := sys.unin[PerUser]; !ok || got.UninstallString == "" || !exists(l.Exe) {
		t.Errorf("entry restored=%v, exe kept=%v; want both", ok, exists(l.Exe))
	}
}

// The entry can't be read: nothing is removed (it couldn't be restored later).
func TestUninstallUnreadableEntryRemovesNothing(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Config, []byte("x: 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	sys.readErr = errors.New("access denied")
	if err := Uninstall(sys, PerUser, true, e); err == nil || !strings.Contains(err.Error(), "read uninstall entry") {
		t.Fatalf("err = %v", err)
	}
	_, entry := sys.unin[PerUser]
	_, run := sys.run[name]
	if !entry || !run || !exists(l.Exe) || !exists(l.StartMenuLink) || !exists(l.Config) {
		t.Errorf("changed: entry=%v run=%v exe=%v link=%v config=%v; want all kept", entry, run, exists(l.Exe), exists(l.StartMenuLink), exists(l.Config))
	}
}

// Files stay and the entry can't be written back: the error says what to delete by hand.
func TestUninstallReportsFailedRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory doesn't stop deletion on Windows")
	}
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.ProgramDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.ProgramDir, 0o755) })
	sys.writeOK = 0
	err = Uninstall(sys, PerUser, false, e)
	if err == nil || !strings.Contains(err.Error(), "couldn't be restored") || !strings.Contains(err.Error(), l.ProgramDir) {
		t.Fatalf("err = %v", err)
	}
}

// A file moved aside whose deletion can't be scheduled is named in the error.
func TestUninstallReportsUnscheduledLeftovers(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	if _, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e}); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(t.TempDir(), "pdb-print.exe.pdb-print.old.1")
	removeDirFn = func(dir string) ([]string, error) { return []string{aside}, os.RemoveAll(dir) }
	t.Cleanup(func() { removeDirFn = removeDir })
	sys.delErr = errors.New("its path has a % in it")
	err := Uninstall(sys, PerUser, false, e)
	if err == nil || !strings.Contains(err.Error(), aside) || !strings.Contains(err.Error(), "uninstalled") {
		t.Fatalf("err = %v", err)
	}
}

// Started with the program dir (or a folder in it) as working dir: it moves out first.
func TestUninstallFromInsideProgramDir(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	l, err := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(l.ProgramDir)
	if err := Uninstall(sys, PerUser, false, e); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd() // compared by name: on macOS it comes back as /private/var/…
	if exists(l.ProgramDir) || filepath.Base(wd) != "Programs" {
		t.Errorf("dir left: %v, cwd %s", exists(l.ProgramDir), wd)
	}
}

func TestWithin(t *testing.T) {
	d := filepath.Join("C", "Programs", "pdb-print")
	for p, want := range map[string]bool{
		d: true, filepath.Join(d, "x"): true, filepath.Join("C", "PROGRAMS", "PDB-print"): true,
		filepath.Join("C", "Programs"): false, d + "2": false, filepath.Join("C", "Programs", "..pdb"): false,
	} {
		if got := within(p, d); got != want {
			t.Errorf("within(%q) = %v", p, got)
		}
	}
}

// An entry that can't be read is an error, not "not installed".
func TestInstalledReadError(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	sys.readErr = errors.New("access denied")
	if _, ok, err := Installed(sys, e); err == nil || ok {
		t.Errorf("ok=%v err=%v; want an error", ok, err)
	}
}

func TestInstalled(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	if _, ok, _ := Installed(sys, e); ok {
		t.Error("nothing installed yet")
	}
	l, _ := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	got, ok, _ := Installed(sys, e)
	if !ok || got.Mode != PerUser || got.ProgramDir != l.ProgramDir || got.Exe != l.Exe {
		t.Errorf("got %+v, %v", got, ok)
	}
}

func TestModeOf(t *testing.T) {
	e := testEnv(t)
	for _, tt := range []struct {
		exe string
		m   Mode
		ok  bool
	}{
		{LayoutFor(Service, e).Exe, Service, true},
		{strings.ToUpper(LayoutFor(Service, e).Exe), Service, true},
		{LayoutFor(PerUser, e).ExeW, PerUser, true},
		{filepath.Join(e.LocalAppData, "Downloads", "pdb-print.exe"), PerUser, false},
		{LayoutFor(Service, e).ProgramDir + "-old" + string(filepath.Separator) + "pdb-print.exe", PerUser, false},
	} {
		if m, ok := ModeOf(tt.exe, e); m != tt.m || ok != tt.ok {
			t.Errorf("%s: %v %v, want %v %v", tt.exe, m, ok, tt.m, tt.ok)
		}
	}
	if _, ok := ModeOf(`pdb-print.exe`, Env{}); ok { // no folders known: never a match
		t.Error("empty env matched")
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\Jöhn Doe\x.exe`: `"C:\Users\Jöhn Doe\x.exe"`,
		`C:\dir\`:                 `"C:\dir\\"`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if !portInUse(addr) {
		t.Error("listening port reported free")
	}
	ln.Close()
	if portInUse(addr) {
		t.Error("closed port reported in use")
	}
	if portInUse("not-an-address") {
		t.Error("malformed address reported in use")
	}
}

func TestListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if !listening(addr) {
		t.Error("listening port not seen")
	}
	ln.Close()
	if listening(addr) {
		t.Error("closed port seen as listening")
	}
}

func TestDeleteLaterCmdLine(t *testing.T) {
	const f = `C:\Users\Jöhn Doe\AppData\Local\Programs\pdb-print.exe.pdb-print.old.7`
	want := `cmd.exe /d /c for /l %i in (1,1,600) do @(ping -n 2 127.0.0.1 >nul & del /f /q "` + f +
		`" >nul 2>&1 & if not exist "` + f + `" exit)`
	if got := DeleteLaterCmdLine(f); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestUninstallEmptyEnvRemovesNothing(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.MkdirAll("pdb-print", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(newFake(), PerUser, true, Env{}); err == nil {
		t.Error("want error")
	}
	if !exists("pdb-print") {
		t.Error("relative dir was removed")
	}
}

// spawnSees records whether the config said self_update: false when the agent was started.
type spawnSees struct {
	*fakeSystem
	path string
	off  *bool
}

func (s spawnSees) Spawn(string, ...string) error {
	c, err := config.Load(s.path, true)
	*s.off = err == nil && c.SelfUpdate != nil && !*c.SelfUpdate
	return nil
}

func TestInstallNoSelfUpdate(t *testing.T) {
	e := testEnv(t)
	path := LayoutFor(PerUser, e).Config
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("log_level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a failed install leaves the config alone
	if _, err := Install(newFake(), Options{Mode: PerUser, NoSelfUpdate: true, Self: filepath.Join(t.TempDir(), exeName), Env: e}); err == nil {
		t.Fatal("install without the exes worked")
	}
	if b, _ := os.ReadFile(path); string(b) != "log_level: debug\n" {
		t.Errorf("config changed by a failed install: %q", b)
	}
	var off bool
	if _, err := Install(spawnSees{newFake(), path, &off}, Options{Mode: PerUser, NoSelfUpdate: true, Self: fakeSelf(t), Env: e}); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path, true)
	if err != nil || !off || c.LogLevel != "debug" {
		t.Errorf("agent saw self_update off: %v; config %+v, %v", off, c, err)
	}
}
