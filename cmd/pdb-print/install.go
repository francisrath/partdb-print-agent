package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	runElevated                 = runElevatedOS // runs pdb-print with args as administrator (UAC) and waits
	stdin             io.Reader = os.Stdin
	stdout            io.Writer = os.Stdout
	// a quitting agent finishes in-flight prints (up to 2 min) before it exits
	exitWait  = 2*time.Minute + 10*time.Second
	portPoll  = 250 * time.Millisecond
	startWait = 10 * time.Second // for the new agent to listen before the browser opens
	quitURL   = "http://" + agentAddr + "/v1/quit"
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
	if *service && !sys.IsAdmin() {
		return errors.New("installing the service needs admin rights: run it as administrator")
	}
	hasService, err := sys.ServiceExists()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	} else if hasService && !*service {
		return errors.New("pdb-print is installed as a service: uninstall that first (pdb-print uninstall, as administrator)")
	}
	if sys.IsAdmin() && !*service {
		fmt.Fprintf(out, "Installing for %s (this window runs as administrator; the agent it starts runs as administrator until you log in again).\n", os.Getenv("USERNAME"))
	}
	stopped := false
	if sys.Listening(agentAddr) {
		l, ok, err := wininstall.Installed(sys, env)
		switch {
		case err != nil:
			return err
		case ok && l.Mode == wininstall.PerUser: // also when the service takes it over
			if err := stopAgent(ctx, sys); err != nil {
				return err
			}
			stopped = true
		case !hasService: // a running service is stopped by Install
			return errors.New("port 8631 is in use: another pdb-print (or another program) is running; stop it and run install again")
		}
	}
	mode := wininstall.PerUser
	if *service {
		mode = wininstall.Service
	}
	l, err := install(sys, env, mode, *quiet, *noSelfUpdate, out)
	if err != nil && stopped && *service { // the per-user install may be gone already
		return fmt.Errorf("%w (the per-user pdb-print was stopped; run the install again)", err)
	}
	if err != nil && stopped {
		w := wininstall.LayoutFor(wininstall.PerUser, env).ExeW
		return fmt.Errorf("%w (the running pdb-print was stopped; start it again with %s serve, or log off and on)", err, wininstall.Quote(w))
	}
	if err != nil {
		return err
	}
	if *service {
		fmt.Fprintf(out, "pdb-print %s is installed in %s and runs as the Windows service pdb-print.\nWeb UI: http://%s (also in the Start menu); to use it in a browser, run pdb-print pair as administrator and type the code it shows.\n", appVersion(), l.ProgramDir, agentAddr)
	} else {
		fmt.Fprintf(out, "pdb-print %s is installed in %s and starts when you log in.\nWeb UI: http://%s (also in the Start menu)\n", appVersion(), l.ProgramDir, agentAddr)
	}
	switch {
	case *quiet:
	case !waitPort(sys, true, startWait):
		fmt.Fprintf(out, "pdb-print is starting; open http://%s in a moment.\n", agentAddr)
	default:
		if err := openBrowser("http://" + agentAddr); err != nil {
			fmt.Fprintln(out, "couldn't open the browser:", err)
		}
	}
	return nil
}

func install(sys wininstall.System, env wininstall.Env, mode wininstall.Mode, quiet, noSelfUpdate bool, out io.Writer) (wininstall.Layout, error) {
	self, err := selfExe()
	if err != nil {
		return wininstall.Layout{}, err
	}
	return wininstall.Install(sys, wininstall.Options{Mode: mode, Quiet: quiet, NoSelfUpdate: noSelfUpdate,
		Self: self, Version: appVersion(), Env: env, Out: out})
}

// errUACDeclined: the user said no to the administrator prompt.
var errUACDeclined = errors.New("administrator rights declined")

// exitCode is a failure whose exit status main passes on.
type exitCode int

func (e exitCode) Error() string {
	return fmt.Sprintf("the uninstall as administrator failed (exit status %d)", int(e))
}

// serviceExit is the service's exit for serve's result err. A failure exits with a service-specific
// code, so the recovery actions restart it; after a stop request it is only logged: the admin stopped it.
func serviceExit(err error, stopping bool, log io.Writer) (svcSpecific bool, code uint32) {
	switch {
	case err == nil:
		return false, 0
	case stopping:
		fmt.Fprintln(log, "error while stopping:", err)
		return false, 0
	}
	return true, 1
}

// serviceCmd is `pdb-print service run --config FILE`, what the Windows service starts.
func serviceCmd(ctx context.Context, args []string) error {
	const use = "usage: pdb-print service run --config FILE (started by the Windows service pdb-print)"
	if len(args) == 0 || args[0] != "run" {
		return errors.New(use)
	}
	fs := flag.NewFlagSet("service run", flag.ContinueOnError)
	cfg := fs.String("config", "", "config file")
	if _, err := parse(fs, args[1:]); err != nil {
		return err
	}
	if *cfg == "" {
		return errors.New(use)
	}
	return runService(ctx, *cfg)
}

// closeDelay is how long a successful uninstall started from Settings → Apps stays on screen.
var closeDelay = 3 * time.Second

// uninstallCmd: started from Settings → Apps, its console window would close before the result
// can be read: on success it stays a few seconds, on an error it waits for Enter.
func uninstallCmd(ctx context.Context, args []string, out io.Writer) error {
	err := uninstall(ctx, args, out)
	if startedByExplorer() {
		if err == nil {
			time.Sleep(closeDelay)
			return nil
		}
		fmt.Fprintln(out, "error:", err)
		fmt.Fprint(out, "Press Enter to close.")
		bufio.NewReader(stdin).ReadString('\n')
	}
	return err
}

func uninstall(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the config")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if goos != "windows" {
		return errors.New("pdb-print uninstall works only on Windows")
	}
	sys, env := newSystem(), envFromOS()
	l, ok, err := wininstall.Installed(sys, env)
	if err != nil {
		return err
	}
	if !ok { // a copy running from a zip is not ours to stop
		fmt.Fprintln(out, "pdb-print is not installed.")
		return nil
	}
	mode := l.Mode
	if mode == wininstall.Service && !sys.IsAdmin() { // e.g. from Settings → Apps: ask for admin rights
		wininstall.LeaveDir(l.ProgramDir) // Settings starts us in it, and the elevated uninstall removes it
		code, err := runElevated(append([]string{"uninstall"}, args...))
		switch {
		case errors.Is(err, errUACDeclined):
			return errors.New("pdb-print is installed as a service: uninstalling it needs administrator rights, and the request was declined")
		case err != nil:
			return fmt.Errorf("pdb-print is installed as a service: run the uninstall as administrator (%w)", err)
		case code != 0:
			return exitCode(code) // the elevated uninstall said why in its own window
		}
		return nil
	}
	if mode == wininstall.PerUser && sys.Listening(agentAddr) { // a service is stopped by Uninstall
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
		fmt.Fprintf(out, "pdb-print is uninstalled. Its config stays in %s (for a reinstall; pdb-print uninstall --purge also removes it).\n", filepath.Dir(wininstall.LayoutFor(mode, env).Config))
	}
	return nil
}

// stopAgent asks the agent on agentAddr to quit and waits until its process has exited
// (in-flight prints done) and the port is free.
func stopAgent(ctx context.Context, sys wininstall.System) error {
	pid, err := quitAgent(ctx)
	if err != nil {
		return fmt.Errorf("port 8631 is in use and pdb-print couldn't stop it: %w (end it in Task Manager, then try again)", err)
	}
	const still = "the pdb-print on port 8631 was asked to stop but is still running; end it in Task Manager (pdb-printw.exe or pdb-print.exe) and try again"
	portLimit := startWait
	if pid == 0 { // no pid in the reply: the port going free is all we can wait for
		portLimit = exitWait
	} else if err := sys.WaitExit(pid, exitWait); errors.Is(err, wininstall.ErrCantWatch) {
		portLimit = exitWait // the same: we may not open it (another user's, or elevated)
	} else if err != nil {
		return fmt.Errorf("%s (%w)", still, err)
	}
	if !waitPort(sys, false, portLimit) {
		return errors.New(still)
	}
	return nil
}

// waitPort polls until agentAddr is busy (or free), at most timeout. Busy is a dial, not a bind,
// which could take the port from the agent that is starting.
func waitPort(sys wininstall.System, busy bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	check := func() bool { return !sys.PortInUse(agentAddr) }
	if busy {
		check = func() bool { return sys.Listening(agentAddr) }
	}
	for !check() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(portPoll)
	}
	return true
}

// postQuit is POST /v1/quit and returns the agent's pid (0 when the reply has none). The header makes it a request no
// web page can send without a preflight.
func postQuit(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, quitURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-PDB-Print", "1")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return 0, fmt.Errorf("/v1/quit: %s", resp.Status)
	}
	var res struct{ PID int }
	if json.NewDecoder(resp.Body).Decode(&res) != nil || res.PID < 0 {
		return 0, nil // it was told to quit all the same; 0 = unknown pid
	}
	return res.PID, nil
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
		fmt.Fprint(stdout, "Press Enter to close.")
		in.ReadString('\n')
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
