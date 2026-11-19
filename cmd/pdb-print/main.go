// Command pdb-print is the Part-DB label print agent.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/report"
	"image/png"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/server"
	"github.com/francisrath/partdb-print-agent/internal/source/agentjobs"
	"github.com/francisrath/partdb-print-agent/internal/source/hotfolder"
)

// version is set by the release build (-ldflags "-X main.version=…").
var version = "dev"

const usage = `usage: pdb-print [--config FILE] [--log-level LEVEL] <command> [args]

commands:
  version                          print the version
  serve [--addr 127.0.0.1:8631]    run the local web UI and bookmarklet endpoint
  render FILE.pdf [--printer ID | --media ID] [-o DIR]
                                   rasterise to PNGs at printhead dots (no printing)
  print FILE.pdf --printer ID [--dry-run OUT.bin]
                                   print a local PDF
  print --profile N --ids LIST [--type part|part_lot|storelocation] [--instance ID] --printer ID
                                   fetch labels from Part-DB and print them
                                   (--printer may be omitted when a route exists for the profile)
  printers list                    list configured printers
  printers status ID               query printer status and loaded media
  printers test ID [--dry-run OUT] print a test label sized for the loaded media
  media                            list known media and the models that take them
  discover [--subnet CIDR]... [--no-usb] [--no-network]
                                   find printers on USB and the local network (read-only)
  report [--identify] [--print] [--raw] [--port N] [--out FILE] [HOST|usb]...
                                   collect what's needed to support a printer into a file for a
                                   GitHub issue (asks before sending anything to a printer)
`

func main() {
	// SIGTERM too: docker stop, systemd and launchd use it; both trigger the graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type app struct {
	cfg     *config.Config
	cfgPath string
	log     *slog.Logger
	svc     *printing.Service
}

func run(ctx context.Context, args []string) error {
	g := flag.NewFlagSet("pdb-print", flag.ContinueOnError)
	g.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := g.String("config", "", "config file (default: $PDB_PRINT_CONFIG, else <OS config dir>/pdb-print/config.yaml)")
	logLevel := g.String("log-level", "", "debug|info|warn|error")
	if err := g.Parse(args); err != nil {
		return err
	}
	if g.NArg() == 0 {
		g.Usage()
		return errors.New("no command")
	}
	cmd, rest := g.Arg(0), g.Args()[1:]
	if cmd == "version" {
		fmt.Printf("pdb-print %s (%s %s/%s)\n", appVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if cmd == "discover" { // needs no config: it's how you find what to put in it
		return discover(ctx, rest)
	}
	if cmd == "report" {
		return reportCmd(ctx, rest)
	}
	if cmd == "media" { // needs no config: it's what config errors point to
		// every medium once, with the models that take it
		db := devicedb.Default()
		var order []driver.Media
		takers := map[string][]string{}
		for _, f := range driver.Families() {
			for _, m := range db.Models(f) {
				for _, md := range db.MediaFor(m) {
					if takers[md.ID] == nil {
						order = append(order, md)
					}
					takers[md.ID] = append(takers[md.ID], m.ID)
				}
			}
		}
		for _, md := range order {
			fmt.Printf("%-10s %-20s %4d×%-4d dots  %s\n", md.ID, md.Name, md.PrintableWidthDots, md.PrintableLengthDots, strings.Join(takers[md.ID], " "))
		}
		return nil
	}

	if *cfgPath == "" {
		*cfgPath = os.Getenv("PDB_PRINT_CONFIG")
	}
	path, explicit := *cfgPath, *cfgPath != ""
	if !explicit {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(path, explicit)
	if err != nil {
		return err
	}
	if err := printing.CheckDrivers(cfg); err != nil {
		return err
	}
	lvl := *logLevel
	if lvl == "" {
		lvl = cfg.LogLevel
	}
	var level slog.Level
	if lvl != "" {
		if err := level.UnmarshalText([]byte(lvl)); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	a := &app{cfg: cfg, cfgPath: path, log: log, svc: &printing.Service{Log: log}}
	pdftoppm := cfg.Pdftoppm
	if v := os.Getenv("PDB_PRINT_PDFTOPPM"); v != "" {
		pdftoppm = v
	}
	if a.svc.Pdftoppm, err = render.FindPdftoppm(pdftoppm); err != nil {
		log.Warn("pdftoppm not usable", "err", err)
	}
	defer a.svc.Close()
	a.log.Debug("config", "path", path)

	switch cmd {
	case "serve":
		return a.serve(ctx, rest)
	case "render":
		return a.render(rest)
	case "print":
		return a.print(ctx, rest)
	case "printers":
		return a.printers(ctx, rest)
	}
	g.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func appVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

// parse lets flags follow positional args ("print x.pdf --printer ql").
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (a *app) render(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	printerID := fs.String("printer", "", "printer id (for media and rotation)")
	mediaID := fs.String("media", "", "media id, e.g. dk-22210 (overrides the printer's)")
	outDir := fs.String("o", ".", "output directory")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("render needs exactly one PDF")
	}
	p := &config.Printer{}
	if *printerID != "" {
		if p, err = a.cfg.Printer(*printerID); err != nil {
			return err
		}
	}
	if *mediaID != "" {
		p.Media = *mediaID
	}
	m, ok := printing.LookupMedia(p, p.Media)
	if !ok {
		return fmt.Errorf("unknown or missing media %q (see `pdb-print media`)", p.Media)
	}
	pdf, err := render.ReadFile(pos[0])
	if err != nil {
		return err
	}
	pages, err := a.svc.Render(pdf, p, &m)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(pos[0]), filepath.Ext(pos[0]))
	for i, img := range pages {
		out := filepath.Join(*outDir, fmt.Sprintf("%s-%d.png", base, i+1))
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			return err
		}
		if err := os.WriteFile(out, b.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("%s %dx%d\n", out, img.Bounds().Dx(), img.Bounds().Dy())
	}
	return nil
}

func (a *app) print(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("print", flag.ContinueOnError)
	printerID := fs.String("printer", "", "printer id from the config")
	dryRun := fs.String("dry-run", "", "write the printer byte stream to this file instead of printing")
	profile := fs.Int("profile", 0, "Part-DB label profile id")
	ids := fs.String("ids", "", `element ids, Part-DB range syntax ("32,112-125")`)
	elemType := fs.String("type", "", "part | part_lot | storelocation (default: the profile's)")
	instance := fs.String("instance", "", "Part-DB instance id (default: the only one)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	fromPartDB := *profile != 0 || *ids != ""
	var in *config.Instance
	if fromPartDB {
		if in, err = a.cfg.Instance(*instance); err != nil {
			return err
		}
		if *printerID == "" {
			if rt, ok := a.cfg.Route(in.ID, *profile); ok {
				*printerID = rt.Printer
			}
		}
	}
	if *printerID == "" || fromPartDB == (len(pos) == 1) || len(pos) > 1 || (fromPartDB && (*profile <= 0 || *ids == "")) {
		return errors.New("usage: print FILE.pdf --printer ID | print --profile N --ids LIST [--type T] --printer ID")
	}
	switch *elemType {
	case "", "part", "part_lot", "storelocation":
	default:
		return fmt.Errorf("--type must be part, part_lot or storelocation")
	}

	var pdf []byte
	if fromPartDB {
		start := time.Now()
		if pdf, err = partdb.FromInstance(in).GenerateLabels(ctx, *profile, *ids, *elemType); err != nil {
			return err
		}
		a.log.Info("fetched labels", "instance", in.ID, "profile", *profile, "ids", *ids, "bytes", len(pdf), "took", time.Since(start).Round(time.Millisecond))
	} else if pdf, err = render.ReadFile(pos[0]); err != nil {
		return err
	}
	return a.printPDF(ctx, *printerID, *dryRun, printing.PDF(pdf))
}

func (a *app) serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8631", "listen address (loopback only)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		// ponytail: LAN bind needs the admin password from architecture §10; not built yet.
		return fmt.Errorf("refusing to listen on %s: only loopback addresses are supported", host)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := server.New(a.cfg, a.cfgPath, a.svc, a.log, ln.Addr().String(), appVersion())
	var folderDone chan struct{} // closed when the hot folder has stopped
	if hf := a.cfg.HotFolder; hf.Enabled {
		dir, err := hf.Path()
		if err != nil {
			return err
		}
		// ponytail: read once at startup; changing hot_folder needs a restart
		folder := &hotfolder.Folder{Dir: dir, Pattern: hf.Pattern, Log: a.log,
			Print: func(ctx context.Context, path string) error {
				return srv.PrintFile(ctx, "hot folder", hf.Printer, path)
			}}
		folderDone = make(chan struct{})
		go func() {
			defer close(folderDone)
			_ = folder.Run(ctx)
		}()
	}
	jobsDone := a.startAgentJobs(ctx, srv)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 2 * time.Minute}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// let in-flight prints finish, so the printer never gets a partial job
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	a.log.Info("web UI ready", "url", "http://"+ln.Addr().String(), "config", a.cfgPath, "renderer", a.renderer())
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped // Serve returns as soon as Shutdown starts; wait for it to finish
	if folderDone != nil {
		<-folderDone // a hot-folder print in progress finishes (or fails) before we exit
	}
	jobsDone.Wait() // same for a print job from Part-DB
	return nil
}

// agentJobs fetches print jobs from Part-DB instances that have a token (and the print agent API),
// one loop per instance.
type agentJobs struct {
	ctx           context.Context
	srv           *server.Server
	log           *slog.Logger
	agentID, name string
	wg            sync.WaitGroup
	mu            sync.Mutex
	stop          map[string]context.CancelFunc // running loop per instance id
}

// startAgentJobs starts a loop for every configured instance and, through srv.OnInstance, for
// instances connected later in the web UI.
func (a *app) startAgentJobs(ctx context.Context, srv *server.Server) *sync.WaitGroup {
	j := &agentJobs{ctx: ctx, srv: srv, log: a.log, stop: map[string]context.CancelFunc{}}
	agentID, err := srv.EnsureAgentID()
	if err != nil {
		a.log.Warn("print jobs disabled: can't save the agent id", "err", err)
		return &j.wg
	}
	j.agentID = agentID
	j.name, _ = os.Hostname()
	srv.OnInstance = j.start
	if len(a.cfg.Instances) == 0 && os.Getenv("PDB_PRINT_URL") == "" {
		a.log.Info("not fetching print jobs", "reason", "no Part-DB connected")
		return &j.wg
	}
	ids := []string{""} // only PDB_PRINT_URL/PDB_PRINT_TOKEN
	if len(a.cfg.Instances) > 0 {
		ids = nil
		for _, in := range a.cfg.Instances {
			ids = append(ids, in.ID)
		}
	}
	for _, id := range ids {
		in, err := a.cfg.Instance(id)
		if err != nil { // not connected yet, or no token
			a.log.Info("not fetching print jobs", "instance", id, "reason", err)
			continue
		}
		j.start(*in)
	}
	return &j.wg
}

// start runs the jobs loop for in, replacing a running one for the same instance
// (a changed URL or token takes effect at once).
func (j *agentJobs) start(in config.Instance) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if stop := j.stop[in.ID]; stop != nil {
		stop()
	}
	ctx, stop := context.WithCancel(j.ctx)
	j.stop[in.ID] = stop
	j.log.Info("fetching print jobs", "instance", in.ID, "url", in.BaseURL)
	loop := &agentjobs.Loop{Client: partdb.FromInstance(&in), AgentID: j.agentID, Name: j.name, Version: appVersion(),
		Printers: j.srv.AgentPrinters, Log: j.log.With("instance", in.ID),
		Print: func(ctx context.Context, job partdb.Job) (string, error) { return j.srv.PrintJob(ctx, in.ID, job) }}
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		_ = loop.Run(ctx)
	}()
}

func (a *app) printPDF(ctx context.Context, printerID, dryRun string, src printing.Source) error {
	p, err := a.cfg.Printer(printerID)
	if err != nil {
		return err
	}
	_, err = a.svc.Print(ctx, p, dryRun, src)
	return err
}

func (a *app) printers(ctx context.Context, args []string) error {
	switch {
	case len(args) == 1 && args[0] == "list":
		for _, p := range a.cfg.Printers {
			where := fmt.Sprintf("%s:%d", p.Transport.Host, p.Transport.Port)
			switch {
			case p.Transport.Kind == "usb" && p.Transport.USBID != "":
				where = "usb " + strings.TrimSpace(p.Transport.USBID+" "+p.Transport.Serial)
			case p.Transport.Kind == "usb":
				where = p.Transport.Device
			}
			fmt.Printf("%-12s %-10s %s media=%s\n", p.ID, p.Model, where, p.Media)
		}
		return nil
	case len(args) == 2 && args[0] == "status":
		p, err := a.cfg.Printer(args[1])
		if err != nil {
			return err
		}
		st, err := a.svc.Status(ctx, p)
		if errors.Is(err, driver.ErrNoReadback) {
			fmt.Println("connected, but the printer sent no status reply")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("model:  %s\nstatus: %s %s\nmedia:  %s\n", st.Model, st.Code, st.Detail, printing.MediaName(st.Media))
		return nil
	case len(args) >= 2 && args[0] == "test":
		fs := flag.NewFlagSet("printers test", flag.ContinueOnError)
		dryRun := fs.String("dry-run", "", "write the printer byte stream to this file instead of printing")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			break
		}
		p, err := a.cfg.Printer(pos[0])
		if err != nil {
			return err
		}
		return a.printPDF(ctx, p.ID, *dryRun, printing.TestCard(p.Model, appVersion()))
	}
	return errors.New("usage: printers list | printers status ID | printers test ID [--dry-run OUT.bin]")
}

// renderer names the PDF renderer jobs start with.
func (a *app) renderer() string {
	if a.svc.Pdftoppm != nil {
		return "pdftoppm (" + a.svc.Pdftoppm.Bin + ")"
	}
	return "pdfium"
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func discover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	var subnets multiFlag
	fs.Var(&subnets, "subnet", "network to scan, e.g. 192.168.1.0/24 (repeatable; default: this computer's networks, each at most a /24)")
	noUSB := fs.Bool("no-usb", false, "skip USB")
	noNet := fs.Bool("no-network", false, "skip the network")
	port := fs.Int("port", 9100, "print port to look for")
	timeout := fs.Duration("timeout", 300*time.Millisecond, "connect timeout per address")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	db := devicedb.Default()
	var found []discovery.Candidate
	if !*noUSB {
		cs, err := discovery.USB(db, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "usb:", err)
		}
		found = append(found, cs...)
	}
	if !*noNet {
		o := discovery.Options{Port: *port, DialTimeout: *timeout}
		for _, s := range subnets {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return fmt.Errorf("--subnet %s: %w", s, err)
			}
			o.Subnets = append(o.Subnets, p)
		}
		cs, err := discovery.Network(ctx, db, o)
		if err != nil {
			return err
		}
		found = append(found, cs...)
	}
	if len(found) == 0 {
		fmt.Println("no printers found")
		return nil
	}
	for _, c := range found {
		addr, what := c.Device, c.ModelID
		if c.Kind == "tcp" {
			addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
		}
		switch {
		case what == "" && c.IEEE1284["MDL"] != "":
			what = strings.TrimSpace(c.IEEE1284["MFG"] + " " + c.IEEE1284["MDL"])
		case what == "" && c.SNMPDescr != "":
			what = c.SNMPDescr
		case what == "":
			what = "?"
		}
		fmt.Printf("%-4s %-22s %-28s %s\n", c.Kind, addr, what, c.Status)
		if c.Status != discovery.Unknown {
			fmt.Printf("     model: %s\n     %s\n", c.ModelID, discovery.TransportLine(c))
		}
	}
	if slices.ContainsFunc(found, func(c discovery.Candidate) bool { return c.Status == discovery.Unknown }) {
		fmt.Println("\nUnknown printers: `pdb-print report` collects what we need to add them.")
	}
	return nil
}

func reportCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	identify := fs.Bool("identify", false, "send identify queries to printers we can't name (asks first)")
	print := fs.Bool("print", false, "print a test label on supported printers (asks first)")
	raw := fs.Bool("raw", false, "don't mask IP/MAC addresses, serial numbers and host names")
	out := fs.String("out", "", "report file (default pdb-print-report-<time>.json)")
	port := fs.String("port", "9100", "print port of network printers")
	targets, err := parse(fs, args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &printing.Service{Log: log}
	defer svc.Close()
	in := bufio.NewReader(os.Stdin)
	r, err := report.Run(ctx, report.Options{
		Version: appVersion(), DB: devicedb.Default(), Targets: targets, Port: *port, Identify: *identify, Print: *print,
		Out: os.Stdout, Svc: svc,
		Confirm: func(q string) bool { return confirm(ctx, in, q) },
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err // Ctrl-C at a prompt: no half report
	}
	b, err := r.JSON(*raw)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = "pdb-print-report-" + time.Now().Format("20060102-150405") + ".json"
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s. Attach it to a GitHub issue for the printer (check it first: addresses and serials are masked unless --raw).\n", *out)
	return nil
}

// confirm asks a y/N question on the terminal; Ctrl-C (ctx done) answers no at once.
func confirm(ctx context.Context, in *bufio.Reader, q string) bool {
	if ctx.Err() != nil {
		return false
	}
	fmt.Printf("%s [y/N] ", q)
	answer := make(chan string, 1)
	go func() { // a terminal read can't be interrupted; it is left to finish on its own
		line, _ := in.ReadString('\n')
		answer <- line
	}()
	select {
	case a := <-answer:
		return strings.EqualFold(strings.TrimSpace(a), "y")
	case <-ctx.Done():
		fmt.Println()
		return false
	}
}
