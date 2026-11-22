//go:build !windows

package wininstall

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type osSystem struct{}

// NewSystem returns the real System; on this platform every method but PortInUse and Listening fails.
func NewSystem() System { return osSystem{} }

func (osSystem) IsAdmin() bool                           { return false }
func (osSystem) SetRun(string, string) error             { return ErrNotWindows }
func (osSystem) DeleteRun(string) error                  { return ErrNotWindows }
func (osSystem) WriteUninstall(Mode, Entry) error        { return ErrNotWindows }
func (osSystem) ReadUninstall(Mode) (Entry, bool, error) { return Entry{}, false, ErrNotWindows }
func (osSystem) DeleteUninstall(Mode) error              { return ErrNotWindows }
func (osSystem) ServiceExists() (bool, error)            { return false, ErrNotWindows }
func (osSystem) InstallService(string, []string) error   { return ErrNotWindows }
func (osSystem) RemoveService() error                    { return ErrNotWindows }
func (osSystem) StartService() error                     { return ErrNotWindows }
func (osSystem) StopService(time.Duration) error         { return ErrNotWindows }
func (osSystem) Spawn(string, ...string) error           { return ErrNotWindows }
func (osSystem) PortInUse(addr string) bool              { return portInUse(addr) }
func (osSystem) Listening(addr string) bool              { return listening(addr) }
func (osSystem) DeleteAfterExit(string) error            { return ErrNotWindows }
func (osSystem) WaitExit(int, time.Duration) error       { return ErrNotWindows }
func (osSystem) OwnedByAdmins(string) (bool, error)      { return false, ErrNotWindows }

func isReparse(fi os.FileInfo) bool { return fi.Mode()&os.ModeSymlink != 0 }

// createAdminOnly creates dir (and its parents) for its owner only; it fails if dir exists.
func createAdminOnly(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	return os.Mkdir(dir, 0o700)
}

// MkdirAdminOnly creates dir (and its parents) for its owner only, or resets an existing one to that.
func MkdirAdminOnly(dir string) error {
	if err := notLink(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// WriteAdminOnly writes data to path readable only by its owner: a new 0600 temp file in the same
// folder, renamed over path, so a wider existing file never holds the data and the write is atomic.
func WriteAdminOnly(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
