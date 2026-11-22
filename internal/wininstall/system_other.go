//go:build !windows

package wininstall

import (
	"errors"
	"syscall"
	"time"
)

type osSystem struct{}

// NewSystem returns the real System; on this platform every method but PortInUse fails.
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
func (osSystem) WaitExit(int, time.Duration) error       { return ErrNotWindows }

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
