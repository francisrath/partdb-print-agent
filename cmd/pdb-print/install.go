package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/wininstall"
)

const agentAddr = "127.0.0.1:8631"

// Swapped by tests: no real registry, processes, network or browser there.
var (
	goos                        = runtime.GOOS
	newSystem                   = wininstall.NewSystem
	envFromOS                   = wininstall.EnvFromOS
	selfExe                     = os.Executable
	quitAgent                   = postQuit
	openBrowser                 = func(url string) error { return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start() }
	startedByExplorer           = startedByExplorerOS
	stdin             io.Reader = os.Stdin
	stdout            io.Writer = os.Stdout
	// a running agent finishes in-flight prints (up to 2 min) before it lets go of the port
	portWait = 2*time.Minute + 10*time.Second
	portPoll = 250 * time.Millisecond
)

func installCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	service := fs.Bool("service", false, "install as a Windows service for all users (needs admin)")
	quiet := fs.Bool("quiet", false, "don't open the web UI afterwards")
	noSelfUpdate := fs.Bool("no-self-update", false, "never update itself to a new release")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if goos != "windows" {
		return errors.New("pdb-print install works only on Windows (elsewhere: see the README)")
	}
	sys, env := newSystem(), envFromOS()
	mode := wininstall.PerUser
	if *service {
		if !sys.IsAdmin() {
			return errors.New("installing the service needs admin rights: run it as administrator")
		}
		mode = wininstall.Service
	} else if ok, err := sys.ServiceExists(); err != nil {
		return fmt.Errorf("query service: %w", err)
	} else if ok {
		return errors.New("pdb-print is installed as a service: uninstall that first (pdb-print uninstall, as administrator)")
	}
	if sys.PortInUse(agentAddr) {
		if _, ok := wininstall.Installed(sys, env); !ok {
			return errors.New("port 8631 is in use: another pdb-print (or another program) is running; stop it and run install again")
		}
		if err := stopAgent(ctx, sys); err != nil {
			return err
		}
	}
	if *noSelfUpdate {
		if err := disableSelfUpdate(wininstall.LayoutFor(mode, env).Config); err != nil {
			return err
		}
	}
	self, err := selfExe()
	if err != nil {
		return err
	}
	l, err := wininstall.Install(sys, wininstall.Options{Mode: mode, Quiet: *quiet, NoSelfUpdate: *noSelfUpdate,
		Self: self, Version: appVersion(), Env: env})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "pdb-print %s is installed in %s and starts when you log in.\nWeb UI: http://%s (also in the Start menu)\n", appVersion(), l.ProgramDir, agentAddr)
	if !*quiet && waitPort(sys, true, 10*time.Second) {
		if err := openBrowser("http://" + agentAddr); err != nil {
			fmt.Fprintln(out, "couldn't open the browser:", err)
		}
	}
	return nil
}

func uninstallCmd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the config")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if goos != "windows" {
		return errors.New("pdb-print uninstall works only on Windows")
	}
	sys, env := newSystem(), envFromOS()
	mode := wininstall.PerUser
	if l, ok := wininstall.Installed(sys, env); ok {
		mode = l.Mode
	}
	if mode == wininstall.Service && !sys.IsAdmin() {
		return errors.New("pdb-print is installed as a service: run it as administrator")
	}
	if sys.PortInUse(agentAddr) {
		if err := stopAgent(ctx, sys); err != nil {
			return err
		}
	}
	if err := wininstall.Uninstall(sys, mode, *purge, env); err != nil {
		return err
	}
	if *purge {
		fmt.Fprintln(out, "pdb-print is uninstalled, its config deleted.")
	} else {
		fmt.Fprintf(out, "pdb-print is uninstalled. Its config stays in %s.\n", filepath.Dir(wininstall.LayoutFor(mode, env).Config))
	}
	return nil
}

// stopAgent asks the agent on agentAddr to quit and waits until it lets go of the port.
func stopAgent(ctx context.Context, sys wininstall.System) error {
	if err := quitAgent(ctx); err != nil {
		return fmt.Errorf("couldn't stop the pdb-print running on port 8631 (end it in Task Manager, then try again): %w", err)
	}
	if !waitPort(sys, false, portWait) {
		return fmt.Errorf("the pdb-print running on port 8631 didn't stop within %s (end it in Task Manager, then try again)", portWait)
	}
	return nil
}

// waitPort polls until agentAddr is busy (or free), at most timeout.
func waitPort(sys wininstall.System, busy bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for sys.PortInUse(agentAddr) != busy {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(portPoll)
	}
	return true
}

// postQuit is POST /v1/quit; the header makes it a request no web page can send without a preflight.
func postQuit(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+agentAddr+"/v1/quit", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-PDB-Print", "1")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("/v1/quit: %s", resp.Status)
	}
	return nil
}

func disableSelfUpdate(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cannot locate the config folder (got %q)", path)
	}
	cfg, err := config.Load(path, false)
	if err != nil {
		return err
	}
	off := false
	cfg.SelfUpdate = &off
	return cfg.Save(path)
}

// doubleClick is what a pdb-print.exe opened from Explorer does: offer the per-user install.
func doubleClick(ctx context.Context) error {
	in := bufio.NewReader(stdin)
	fmt.Fprint(stdout, "Install pdb-print for this user? It will start when you log in. [Y/n] ")
	line, _ := in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
	default:
		fmt.Fprint(stdout, usage)
		return nil
	}
	err := installCmd(ctx, nil, stdout)
	if err != nil {
		fmt.Fprintln(stdout, "error:", err) // main prints it too, but only as the window closes
	}
	fmt.Fprint(stdout, "Press Enter to close.")
	in.ReadString('\n')
	return err
}
