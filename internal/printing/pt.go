package printing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherpt"
)

// ptPageTimeout bounds the wait for one label's "printing completed": a 1 m label at the
// PT-9500PC's ~20 mm/s takes about 50 s.
const ptPageTimeout = 90 * time.Second

// printPT prints on a brother-pt printer over its transport (USB). The printer needs the
// status before the job and each page confirmed before the next is sent (reference p.2).
// The caller holds the printer lock.
func (s *Service) printPT(ctx context.Context, p *config.Printer, dryRun string, source Source) (*Result, error) {
	drv := brotherpt.New()
	if _, err := drv.Capabilities(p.Model); err != nil {
		return nil, err
	}
	media, ok := brotherpt.LookupMedia(p.Media)
	if p.Media != "" && !ok {
		return nil, fmt.Errorf("unknown media %q (see `pdb-print media`)", p.Media)
	}

	var t driver.Transport
	mediaType := ""
	if dryRun == "" {
		t = s.transport(p)
		if err := t.Open(ctx); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		defer t.Close()
		st, err := drv.Status(ctx, t)
		if errors.Is(err, driver.ErrNoReadback) {
			return nil, &driver.Error{Code: driver.CommsError, Detail: "printer sent no status reply"}
		}
		if err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		s.Log.Info("printer status", "model", st.Model, "code", st.Code, "media", MediaName(st.Media), "type", st.MediaType)
		if st.Code != driver.OK {
			return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
		}
		switch {
		case st.Media == nil:
		case st.Media.ID == "unknown":
			s.Log.Warn("printer reports a tape this agent doesn't know; using the configured media", "loaded", MediaName(st.Media))
		case p.Media != "" && st.Media.ID != p.Media:
			return nil, &driver.Error{Code: driver.MediaMismatch,
				Detail: fmt.Sprintf("config says %s, printer has %s loaded", p.Media, MediaName(st.Media))}
		case p.Media == "":
			media, ok = *st.Media, true
		}
		mediaType = st.MediaType
	}
	if !ok {
		return nil, errors.New("media unknown: set `media:` for this printer in the config")
	}

	start := time.Now()
	pages, err := s.rasterise(p, &media, source)
	if err != nil {
		return nil, err
	}
	cut := ptCut(p.Cut, len(pages), mediaType, dryRun != "")
	if cut != p.Cut && p.Cut != "" {
		s.Log.Warn("half cut needs laminated tape; cutting each label instead", "printer", p.ID, "tape", mediaType)
	}
	header, per, err := drv.EncodePages(pages, &driver.Job{Cut: cut}, &media)
	if err != nil {
		return nil, err
	}
	res := &Result{Pages: len(pages), Media: media.ID, Bytes: len(header)}
	for _, pg := range per {
		res.Bytes += len(pg)
	}
	if dryRun != "" {
		return res, os.WriteFile(dryRun, bytes.Join(append([][]byte{header}, per...), nil), 0o644)
	}

	if _, err := t.Write(header); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	for i, pg := range per {
		if _, err := t.Write(pg); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		err := brotherpt.WaitPage(ctx, t, ptPageTimeout)
		var de *driver.Error
		switch {
		case errors.As(err, &de):
			return nil, de
		case errors.Is(err, driver.ErrNoReadback) && i == len(per)-1:
			s.Log.Info("sent (unconfirmed: no completion status received)", "printer", p.ID, "pages", len(pages))
			return res, nil
		case errors.Is(err, driver.ErrNoReadback):
			// sending more while it may still print could damage the printer (reference p.2)
			return nil, &driver.Error{Code: driver.CommsError, Detail: fmt.Sprintf("label %d: printer never reported it printed", i+1)}
		case err != nil:
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
	}
	res.Confirmed = true
	s.Log.Info("printed", "printer", p.ID, "pages", len(pages), "cut", cut, "bytes", res.Bytes, "took", time.Since(start).Round(time.Millisecond))
	return res, nil
}

// ptCut resolves the cut mode. Default: half cut between labels of a batch on laminated
// tape (the strip stays whole, one full cut at the end), otherwise a full cut per label.
// Half cut only works on laminated tape (reference p.20); on a dry run the tape is unknown,
// so an explicit "half" is kept.
func ptCut(cut string, pages int, mediaType string, dryRun bool) string {
	laminated := mediaType == "laminated" || mediaType == "hg"
	switch {
	case cut == "" && pages > 1 && laminated:
		return "half"
	case cut == "":
		return "per_label"
	case cut == "half" && !laminated && !dryRun:
		return "per_label"
	}
	return cut
}
