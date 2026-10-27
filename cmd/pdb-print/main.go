// Command pdb-print is the Part-DB label print agent.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherql"
	"github.com/francisrath/partdb-print-agent/internal/render"
	"github.com/francisrath/partdb-print-agent/internal/transport/tcp"
)

// version is set by the release build (-ldflags "-X main.version=…").
var version = "dev"

const usage = `usage: pdb-print [--config FILE] [--log-level LEVEL] <command> [args]

commands:
  version                          print the version
  render FILE.pdf [--printer ID | --media ID] [-o DIR]
                                   rasterise to PNGs at printhead dots (no printing)
  print FILE.pdf --printer ID [--dry-run OUT.bin]
                                   print a local PDF
  printers list                    list configured printers
  printers status ID               query printer status and loaded media
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
	cfg *config.Config
	log *slog.Logger
}

func run(ctx context.Context, args []string) error {
	g := flag.NewFlagSet("pdb-print", flag.ContinueOnError)
	g.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := g.String("config", "", "config file (default: OS config dir)/pdb-print/config.yaml")
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
	a := &app{cfg: cfg, log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))}
	a.log.Debug("config", "path", path)

	switch cmd {
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
	pages, err := renderFile(pos[0], p, &m)
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

func renderFile(path string, p *config.Printer, m *driver.Media) ([]*image.Gray, error) {
	pdf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := render.New()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Render(pdf, render.Options{
		WidthDots:    m.PrintableWidthDots,
		LengthDots:   m.PrintableLengthDots,
		MediaWidthMM: m.WidthMM,
		Rotate:       p.Rotate,
		Threshold:    p.Threshold,
	})
}

func (a *app) print(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("print", flag.ContinueOnError)
	printerID := fs.String("printer", "", "printer id from the config")
	dryRun := fs.String("dry-run", "", "write the printer byte stream to this file instead of printing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *printerID == "" {
		return errors.New("usage: print FILE.pdf --printer ID")
	}
	p, err := a.cfg.Printer(*printerID)
	if err != nil {
		return err
	}
	drv := brotherql.New()
	if _, err := drv.Capabilities(p.Model); err != nil {
		return err
	}

	var t *tcp.Transport
	readback := false
	media, ok := brotherql.LookupMedia(p.Media)
	if p.Media != "" && !ok {
		return fmt.Errorf("unknown media %q", p.Media)
	}
	if *dryRun == "" {
		t = tcp.New(net.JoinHostPort(p.Transport.Host, strconv.Itoa(p.Transport.Port)))
		if err := t.Open(ctx); err != nil {
			return fmt.Errorf("%w: %v", &driver.Error{Code: driver.CommsError}, err)
		}
		defer t.Close()
		st, err := drv.Status(ctx, t)
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			a.log.Warn("printer sent no status; printing blind", "printer", p.ID)
		case err != nil:
			return err
		default:
			readback = true
			a.log.Info("printer status", "model", st.Model, "code", st.Code, "media", mediaName(st.Media))
			if st.Code != driver.OK {
				return &driver.Error{Code: st.Code, Detail: st.Detail}
			}
			if st.Media != nil && p.Media != "" && st.Media.ID != p.Media {
				return &driver.Error{Code: driver.MediaMismatch,
					Detail: fmt.Sprintf("config says %s, printer has %s loaded", p.Media, mediaName(st.Media))}
			}
			if p.Media == "" && st.Media != nil {
				media, ok = brotherql.LookupMedia(st.Media.ID)
			}
		}
	}
	if !ok {
		return errors.New("media unknown: set `media:` for this printer in the config")
	}

	start := time.Now()
	pages, err := renderFile(pos[0], p, &media)
	if err != nil {
		return &driver.Error{Code: driver.RenderError, Detail: err.Error()}
	}
	a.log.Info("rendered", "pages", len(pages), "media", media.ID, "took", time.Since(start).Round(time.Millisecond))

	var job bytes.Buffer
	if err := drv.Encode(ctx, &job, pages, &driver.Job{Cut: p.Cut}, &media); err != nil {
		return err
	}
	if *dryRun != "" {
		return os.WriteFile(*dryRun, job.Bytes(), 0o644)
	}
	if _, err := t.Write(job.Bytes()); err != nil {
		return fmt.Errorf("%w: %v", &driver.Error{Code: driver.CommsError}, err)
	}
	msg := "sent (unconfirmed: printer gives no status)"
	if readback {
		err := brotherql.WaitDone(ctx, t, len(pages), 30*time.Second+time.Duration(len(pages))*5*time.Second)
		switch {
		case errors.Is(err, driver.ErrNoReadback):
			msg = "sent (unconfirmed: no completion status received)"
		case err != nil:
			return err
		default:
			msg = "printed"
		}
	}
	a.log.Info(msg, "pages", len(pages), "bytes", job.Len(), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func mediaName(m *driver.Media) string {
	if m == nil {
		return "none"
	}
	if m.ID == "unknown" {
		return fmt.Sprintf("unknown %s %gx%g mm", m.Kind, m.WidthMM, m.LengthMM)
	}
	return m.ID
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
		t := tcp.New(net.JoinHostPort(p.Transport.Host, strconv.Itoa(p.Transport.Port)))
		if err := t.Open(ctx); err != nil {
			return err
		}
		defer t.Close()
		st, err := brotherql.New().Status(ctx, t)
		if errors.Is(err, driver.ErrNoReadback) {
			fmt.Println("connected, but the printer sent no status reply")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("model:  %s\nstatus: %s %s\nmedia:  %s\n", st.Model, st.Code, st.Detail, mediaName(st.Media))
		return nil
	}
	return errors.New("usage: printers list | printers status ID")
}
