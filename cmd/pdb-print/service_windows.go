package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/francisrath/pdb-print/internal/logfile"
	"github.com/francisrath/pdb-print/internal/wininstall"
)

// runService runs serve as the Windows service pdb-print, logging to pdb-print.log next to the config
// (%ProgramData%\pdb-print). A stop from the service manager shuts serve down as SIGTERM does:
// prints in progress finish first.
func runService(ctx context.Context, cfgPath string) error {
	if ok, err := svc.IsWindowsService(); err != nil {
		return err
	} else if !ok {
		return errors.New("pdb-print service run is what the Windows service pdb-print starts, not a command to type: use pdb-print serve")
	}
	// before the log: an admin may have deleted the dir, and the log would recreate it with the
	// inherited ACL (every user could write to it); one an ordinary user made isn't used at all
	if err := wininstall.ReadyServiceDir(wininstall.NewSystem(), cfgPath); err != nil {
		refuse(cfgPath, err)
		return err // main logs it to stderr, which nobody sees
	}
	logOut = fileLog(filepath.Join(filepath.Dir(cfgPath), "pdb-print.log"))
	serviceMode = true
	h := &service{serve: func(ctx context.Context) error { return run(ctx, []string{"--config", cfgPath, "serve"}) }, ctx: ctx}
	if err := svc.Run("pdb-print", h); err != nil {
		return err
	}
	return h.err // main logs it
}

// refuse records why the service doesn't start: in the Event Log (Windows Logs → Application, source
// pdb-print) and in the log file, but never into a folder that isn't trusted (or is gone): SYSTEM
// writing a file in a folder another user controls could be sent through a link to any file.
func refuse(cfgPath string, err error) {
	if l, lerr := eventlog.Open("pdb-print"); lerr == nil {
		_ = l.Error(1, "The pdb-print service didn't start: "+err.Error())
		l.Close()
	}
	dir := filepath.Dir(cfgPath)
	if fi, serr := os.Lstat(dir); serr == nil && fi.IsDir() && !errors.Is(err, wininstall.ErrUntrustedDir) {
		fmt.Fprintln(&logfile.Writer{Path: filepath.Join(dir, "pdb-print.log"), Max: 10 << 20}, "error: the service didn't start:", err)
	}
}

type service struct {
	ctx   context.Context
	serve func(context.Context) error
	err   error
}

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx) }()
	// preshutdown: at system shutdown, Windows waits for it as long as the service's preshutdown timeout
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown}
	stopping := false
	for {
		select {
		case err := <-done:
			ssec, code := serviceExit(err, stopping, logOut)
			if ssec {
				s.err = err // main logs it
			}
			return ssec, code
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown, svc.PreShutdown:
				// serve waits up to 2 min for a print in progress
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(exitWait.Milliseconds())}
				stopping = true
				cancel()
			}
		}
	}
}

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size                    uint32
	mask                    uint32
	hwnd                    windows.Handle
	verb, file, params, dir *uint16
	show                    int32
	instApp                 windows.Handle
	idList                  uintptr
	class                   *uint16
	keyClass                windows.Handle
	hotKey                  uint32
	iconOrMonitor, process  windows.Handle
}

// runElevatedOS runs this exe with args through the UAC prompt ("runas"), waits, and returns its exit code.
func runElevatedOS(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	params, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return 0, err
	}
	// ShellExecuteEx may use COM (per its docs): initialise it on this thread for the call
	const seeMaskNoCloseProcess = 0x40
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, verb: verb, file: file, params: params, show: windows.SW_SHOWNORMAL}
	info.size = uint32(unsafe.Sizeof(info))
	runtime.LockOSThread()
	com := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE)
	r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if com == nil || com == syscall.Errno(1) { // S_FALSE: already initialised; either way paired
		windows.CoUninitialize() // before the wait below, which may be long
	}
	runtime.UnlockOSThread()
	if r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, errUACDeclined
		}
		return 0, err
	}
	if info.process == 0 {
		return 0, errors.New("the elevated pdb-print didn't start")
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}
