//go:build windows

package wininstall

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	runKey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\` + name
)

type osSystem struct{}

// NewSystem returns the real System backed by the registry and the service manager.
func NewSystem() System { return osSystem{} }

func (osSystem) IsAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

func (osSystem) SetRun(n, cmdline string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(n, cmdline)
}

func (osSystem) DeleteRun(n string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(n); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func root(m Mode) registry.Key {
	if m == Service {
		return registry.LOCAL_MACHINE
	}
	return registry.CURRENT_USER
}

func (osSystem) WriteUninstall(m Mode, e Entry) error {
	k, _, err := registry.CreateKey(root(m), uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for n, v := range map[string]string{
		"DisplayName": e.DisplayName, "DisplayVersion": e.DisplayVersion, "Publisher": e.Publisher,
		"InstallLocation": e.InstallLocation, "UninstallString": e.UninstallString, "DisplayIcon": e.DisplayIcon,
	} {
		if err := k.SetStringValue(n, v); err != nil {
			return err
		}
	}
	for _, n := range []string{"NoModify", "NoRepair"} {
		if err := k.SetDWordValue(n, 1); err != nil {
			return err
		}
	}
	return nil
}

func (osSystem) ReadUninstall(m Mode) (Entry, bool, error) {
	k, err := registry.OpenKey(root(m), uninstallKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return Entry{}, false, nil
	} else if err != nil {
		return Entry{}, false, err
	}
	defer k.Close()
	var e Entry
	for _, v := range []struct {
		name string
		to   *string
	}{{"DisplayName", &e.DisplayName}, {"DisplayVersion", &e.DisplayVersion}, {"Publisher", &e.Publisher},
		{"InstallLocation", &e.InstallLocation}, {"UninstallString", &e.UninstallString}, {"DisplayIcon", &e.DisplayIcon}} {
		s, _, err := k.GetStringValue(v.name)
		if err != nil { // a partial entry written back would leave Settings with a broken uninstall
			return Entry{}, false, fmt.Errorf("%s: %w", v.name, err)
		}
		*v.to = s
	}
	return e, true, nil
}

func (osSystem) DeleteUninstall(m Mode) error {
	if err := registry.DeleteKey(root(m), uninstallKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func connect() (*mgr.Mgr, error) { return mgr.Connect() }

// ServiceExists asks with the least rights there are, so it works without admin
// (mgr.Connect and OpenService ask for all access and are denied to normal users).
func (osSystem) ServiceExists() (bool, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, err
	}
	defer windows.CloseServiceHandle(m)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	windows.CloseServiceHandle(s)
	return true, nil
}

// InstallService creates the service, or brings an existing one's config to what a new one gets.
func (osSystem) InstallService(exe string, args []string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	c := mgr.Config{DisplayName: name, Description: "Part-DB label print agent", StartType: mgr.StartAutomatic}
	s, err := m.OpenService(name)
	if err == nil {
		if err := resetConfig(s, exe, args, c.Description); err != nil {
			s.Close()
			return err
		}
	} else if s, err = m.CreateService(name, exe, c, args...); err != nil {
		return err
	}
	defer s.Close()
	r := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{r, r, r}, 86400); err != nil {
		return err
	}
	return s.SetRecoveryActionsOnNonCrashFailures(true) // also when serve fails (exit code 1), e.g. port busy at boot
}

// resetConfig gives an existing service what CreateService gives a new one: own process, automatic
// start (not delayed), normal error control, LocalSystem, no dependencies, the command line, names.
// (mgr's UpdateConfig can't clear dependencies.)
func resetConfig(s *mgr.Service, exe string, args []string, desc string) error {
	cmd := syscall.EscapeArg(exe) // as CreateService builds it
	for _, a := range args {
		cmd += " " + syscall.EscapeArg(a)
	}
	var p [4]*uint16
	for i, v := range []string{cmd, "LocalSystem", "", name} {
		var err error
		if p[i], err = windows.UTF16PtrFromString(v); err != nil {
			return err
		}
	}
	noDeps := []uint16{0, 0}
	if err := windows.ChangeServiceConfig(s.Handle, windows.SERVICE_WIN32_OWN_PROCESS, windows.SERVICE_AUTO_START,
		windows.SERVICE_ERROR_NORMAL, p[0], nil, nil, &noDeps[0], p[1], p[2], p[3]); err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(desc)
	if err != nil {
		return err
	}
	sd := windows.SERVICE_DESCRIPTION{Description: d}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_DESCRIPTION, (*byte)(unsafe.Pointer(&sd))); err != nil {
		return err
	}
	delayed := windows.SERVICE_DELAYED_AUTO_START_INFO{IsDelayedAutoStartUp: 0}
	return windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_DELAYED_AUTO_START_INFO, (*byte)(unsafe.Pointer(&delayed)))
}

func withService(f func(*mgr.Service) error) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}

func (osSystem) RemoveService() error {
	return withService(func(s *mgr.Service) error { return s.Delete() })
}

func (osSystem) StartService() error {
	return withService(func(s *mgr.Service) error { return s.Start() })
}

func (osSystem) StopService(timeout time.Duration) error {
	return withService(func(s *mgr.Service) error {
		// Stop is sent until the service says it isn't running: one that is starting (or already
		// stopping) can't take it yet and answers CANNOT_ACCEPT_CTRL.
		for end := time.Now().Add(timeout); ; {
			st, err := s.Control(svc.Stop)
			switch {
			case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
				return nil // stopped
			case err == nil && st.State == svc.Stopped:
				return nil
			case err != nil && !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
				return err
			}
			if time.Now().After(end) {
				return errors.New("timeout waiting for service to stop")
			}
			time.Sleep(300 * time.Millisecond)
		}
	})
}

func (osSystem) Spawn(exe string, args ...string) error {
	return startDetached(func() *exec.Cmd {
		c := exec.Command(exe, args...)
		c.Dir = filepath.Dir(exe) // not the installer's folder, which the user may want to delete
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
		return c
	})
}

func (osSystem) DeleteAfterExit(path string) error {
	if strings.Contains(path, "%") { // cmd would expand it
		return errors.New("its path has a % in it")
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	return startDetached(func() *exec.Cmd {
		c := exec.Command(comspec)
		c.Dir = filepath.Dir(path) // the folder that holds it, not the one being removed
		c.SysProcAttr = &syscall.SysProcAttr{CmdLine: DeleteLaterCmdLine(path), CreationFlags: windows.CREATE_NO_WINDOW}
		return c
	})
}

// startDetached starts newCmd() outside our job object, so it survives the job closing (an
// OpenSSH session kills its job on logout). A job that forbids breakaway denies the start; then
// it starts inside the job.
func startDetached(newCmd func() *exec.Cmd) error {
	err := start(newCmd(), windows.CREATE_BREAKAWAY_FROM_JOB)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		err = start(newCmd(), 0)
	}
	return err
}

func start(c *exec.Cmd, flags uint32) error {
	c.SysProcAttr.CreationFlags |= flags
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

func (osSystem) PortInUse(addr string) bool { return portInUse(addr) }
func (osSystem) Listening(addr string) bool { return listening(addr) }

func (osSystem) WaitExit(pid int, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil // no such process: it has already exited
	} else if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return ErrCantWatch // another user's or an elevated process
	} else if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	switch {
	case err != nil:
		return err
	case ev == uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("process %d still running after %s", pid, timeout)
	}
	return nil
}

func (osSystem) OwnedByAdmins(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	o, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	return o.IsWellKnown(windows.WinBuiltinAdministratorsSid) || o.IsWellKnown(windows.WinLocalSystemSid), nil
}

func isReparse(fi os.FileInfo) bool {
	a, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return fi.Mode()&os.ModeSymlink != 0 || ok && a.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, windows.WSAEADDRINUSE)
}

// adminOnlyDir: owned by Administrators; Administrators and SYSTEM, inherited by what is created
// inside; nothing from the parent.
const adminOnlyDir = "O:BAD:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)"

// MkdirAdminOnly creates dir with an ACL of Administrators + SYSTEM only (set at creation, so no
// other user can get a file into it first), or resets an existing dir's owner and ACL to that.
func MkdirAdminOnly(dir string) error {
	if err := notLink(dir); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(adminOnlyDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err // nil: created with the ACL
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("can't take over %s (made by another user without access for administrators): delete it and run the install again", dir)
	}
	return err
}

// WriteAdminOnly writes data to path readable only by Administrators and SYSTEM. The file is
// created under a temporary name with that ACL already set (and no sharing), so no other user can
// open it at any point, then renamed over path (a rename keeps the ACL).
func WriteAdminOnly(path string, data []byte) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;BA)(A;;FA;;;SY)")
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	p, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.PathError{Op: "create", Path: tmp, Err: err}
	}
	f := os.NewFile(uintptr(h), tmp)
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
