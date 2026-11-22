package wininstall

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSystem struct {
	run     map[string]string
	unin    map[Mode]Entry
	service bool
	spawns  [][]string
}

func newFake() *fakeSystem {
	return &fakeSystem{run: map[string]string{}, unin: map[Mode]Entry{}}
}

func (f *fakeSystem) IsAdmin() bool                         { return true }
func (f *fakeSystem) SetRun(n, c string) error              { f.run[n] = c; return nil }
func (f *fakeSystem) DeleteRun(n string) error              { delete(f.run, n); return nil }
func (f *fakeSystem) WriteUninstall(m Mode, e Entry) error  { f.unin[m] = e; return nil }
func (f *fakeSystem) DeleteUninstall(m Mode) error          { delete(f.unin, m); return nil }
func (f *fakeSystem) ServiceExists() (bool, error)          { return f.service, nil }
func (f *fakeSystem) InstallService(string, []string) error { f.service = true; return nil }
func (f *fakeSystem) RemoveService() error                  { f.service = false; return nil }
func (f *fakeSystem) StartService() error                   { return nil }
func (f *fakeSystem) StopService(time.Duration) error       { return nil }
func (f *fakeSystem) PortInUse(string) bool                 { return false }
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
	if en.DisplayVersion != "1.2.3" || en.UninstallString != Quote(l.Exe)+" uninstall" || en.InstallLocation != l.ProgramDir {
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
	for i := 0; i < 2; i++ {
		if _, err := Install(sys, o); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	if len(sys.run) != 1 {
		t.Errorf("run values = %v", sys.run)
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
	if _, err := Install(newFake(), Options{Mode: PerUser, Self: self, Env: testEnv(t)}); err == nil || !strings.Contains(err.Error(), "pdb-printw.exe") {
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
		os.MkdirAll(filepath.Dir(l.Config), 0o755)
		os.WriteFile(l.Config, []byte("x: 1"), 0o600)
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

func TestInstalled(t *testing.T) {
	sys, e := newFake(), testEnv(t)
	if _, ok := Installed(sys); ok {
		t.Error("nothing installed yet")
	}
	l, _ := Install(sys, Options{Mode: PerUser, Self: fakeSelf(t), Version: "1", Env: e})
	got, ok := Installed(sys)
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
}
