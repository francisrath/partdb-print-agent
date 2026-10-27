// Command pdb-print is the Part-DB label print agent.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherql"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/server"
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
  media                            list known Brother QL media
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
	case "media":
		c, _ := brotherql.New().Capabilities("QL-810W")
		for _, m := range c.Media {
			fmt.Printf("%-10s %-20s %4d×%-4d dots\n", m.ID, m.Name, m.PrintableWidthDots, m.PrintableLengthDots)
		}
		return nil
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
	m, ok := brotherql.LookupMedia(p.Media)
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
		go folder.Run(ctx)
	}
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
	a.log.Info("web UI ready", "url", "http://"+ln.Addr().String(), "config", a.cfgPath)
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped // Serve returns as soon as Shutdown starts; wait for it to finish
	return nil
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
			fmt.Printf("%-10s %-10s %-10s %s:%d media=%s\n", p.ID, p.Driver, p.Model, p.Transport.Host, p.Transport.Port, p.Media)
		}
		return nil
	case len(args) == 2 && args[0] == "status":
		p, err := a.cfg.Printer(args[1])
		if err != nil {
			return err
		}
		st, err := printing.Status(ctx, p)
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
