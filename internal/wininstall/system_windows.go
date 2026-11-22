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

func (osSystem) InstallService(exe string, args []string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.CreateService(name, exe, mgr.Config{DisplayName: name, StartType: mgr.StartAutomatic}, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	r := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	return s.SetRecoveryActions([]mgr.RecoveryAction{r, r, r}, 86400)
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
		st, err := s.Control(svc.Stop)
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil // already stopped
		}
		if err != nil {
			return err
		}
		for end := time.Now().Add(timeout); st.State != svc.Stopped; {
			if time.Now().After(end) {
				return errors.New("timeout waiting for service to stop")
			}
			time.Sleep(300 * time.Millisecond)
			if st, err = s.Query(); err != nil {
				return err
			}
		}
		return nil
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

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, windows.WSAEADDRINUSE)
}
