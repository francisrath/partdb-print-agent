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
	run     map[string]string
	unin    map[Mode]Entry
	service bool
	spawns  [][]string
	uninErr error // DeleteUninstall fails with it
}

func newFake() *fakeSystem {
	return &fakeSystem{run: map[string]string{}, unin: map[Mode]Entry{}}
}

func (f *fakeSystem) IsAdmin() bool                        { return true }
func (f *fakeSystem) SetRun(n, c string) error             { f.run[n] = c; return nil }
func (f *fakeSystem) DeleteRun(n string) error             { delete(f.run, n); return nil }
func (f *fakeSystem) WriteUninstall(m Mode, e Entry) error { f.unin[m] = e; return nil }
func (f *fakeSystem) DeleteUninstall(m Mode) error {
	if f.uninErr != nil {
		return f.uninErr
	}
	delete(f.unin, m)
	return nil
}
func (f *fakeSystem) ServiceExists() (bool, error)          { return f.service, nil }
func (f *fakeSystem) InstallService(string, []string) error { f.service = true; return nil }
func (f *fakeSystem) RemoveService() error                  { f.service = false; return nil }
func (f *fakeSystem) StartService() error                   { return nil }
func (f *fakeSystem) StopService(time.Duration) error       { return nil }
func (f *fakeSystem) PortInUse(string) bool                 { return false }
func (f *fakeSystem) Listening(string) bool                 { return false }
func (f *fakeSystem) DeleteAfterExit(string) error          { return nil }
func (f *fakeSystem) WaitExit(int, time.Duration) error     { return nil }
func (f *fakeSystem) Spawn(exe string, args ...string) error {
	f.spawns = append(f.spawns, append([]string{exe}, args...))
	return nil
}
func (f *fakeSystem) ReadUninstall(m Mode) (Entry, bool, error) {
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

func TestInstallServiceNotYet(t *testing.T) {
	if _, err := Install(newFake(), Options{Mode: Service, Self: fakeSelf(t), Env: testEnv(t)}); err == nil {
		t.Error("want error")
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

func TestInstalled(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	if _, ok := Installed(sys, e); ok {
		t.Error("nothing installed yet")
	}
	l, _ := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	got, ok := Installed(sys, e)
	if !ok || got.Mode != PerUser || got.ProgramDir != l.ProgramDir || got.Exe != l.Exe {
		t.Errorf("got %+v, %v", got, ok)
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
