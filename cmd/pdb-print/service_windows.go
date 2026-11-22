package main

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
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
	logOut = fileLog(filepath.Join(filepath.Dir(cfgPath), "pdb-print.log"))
	serviceMode = true
	h := &service{serve: func(ctx context.Context) error { return run(ctx, []string{"--config", cfgPath, "serve"}) }, ctx: ctx}
	if err := svc.Run("pdb-print", h); err != nil {
		return err
	}
	return h.err // main logs it
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
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case s.err = <-done:
			if s.err != nil {
				return true, 1 // a failure: the recovery actions restart it
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// serve waits up to 2 min for a print in progress
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(exitWait.Milliseconds())}
				cancel()
			}
		}
	}
}
