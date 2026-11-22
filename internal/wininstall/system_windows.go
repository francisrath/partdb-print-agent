//go:build windows

package wininstall

import (
	"errors"
	"os/exec"
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
		"InstallLocation": e.InstallLocation, "UninstallString": e.UninstallString,
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
	get := func(n string) string { s, _, _ := k.GetStringValue(n); return s }
	return Entry{get("DisplayName"), get("DisplayVersion"), get("Publisher"), get("InstallLocation"), get("UninstallString")}, true, nil
}

func (osSystem) DeleteUninstall(m Mode) error {
	if err := registry.DeleteKey(root(m), uninstallKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func connect() (*mgr.Mgr, error) { return mgr.Connect() }

func (osSystem) ServiceExists() (bool, error) {
	m, err := connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	s.Close()
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
	c := exec.Command(exe, args...)
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

func (osSystem) PortInUse(addr string) bool { return portInUse(addr) }
