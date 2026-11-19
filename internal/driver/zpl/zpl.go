package zpl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() {
	driver.Register("zpl", func(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
		return &zpl{model: m, opts: o}, nil
	})
}

// Completion polling; tests shorten them.
var (
	pollEvery    = 250 * time.Millisecond
	notSeenAfter = 2 * time.Second // job never seen in the buffer by then: "probably printed"
)

type zpl struct {
	model *devicedb.Model
	opts  driver.Options
}

var errSilent = errors.New("no reply")

// query sends cmd on a connection of its own (Zebras answer each query on a fresh connection;
// one per query is what the GK420d was tested with) and reads until etx replies have ended.
func query(ctx context.Context, t driver.Transport, cmd string, etx int) ([]byte, error) {
	if err := t.Open(ctx); err != nil {
		return nil, err
	}
	defer t.Close()
	if _, err := t.Write([]byte(cmd + "\r\n")); err != nil {
		return nil, err
	}
	var reply []byte
	buf := make([]byte, 64<<10)
	for bytes.Count(reply, []byte{0x03}) < etx {
		n, err := t.Read(buf)
		reply = append(reply, buf[:n]...)
		if err != nil {
			if len(reply) == 0 {
				return nil, errSilent
			}
			if !errors.Is(err, io.EOF) && bytes.Count(reply, []byte{0x03}) == 0 {
				return reply, err
			}
			break
		}
	}
	return reply, nil
}

// Status reads ~HQES (errors by name; missing on old firmware) and ~HS (flags, label length, the
// print buffer). A Zebra doesn't answer ~HS at all while its head is open or it is out of labels.
func (z *zpl) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	var hq *hqes
	if r, err := query(ctx, t, "~HQES", 1); err == nil {
		if q, err := parseHQES(r); err == nil {
			hq = &q
		}
	}
	r, err := query(ctx, t, "~HS", 3)
	var h hs
	if err == nil {
		h, err = parseHS(r)
	}
	if err != nil {
		if hq != nil {
			if code, detail := hq.code(); code != driver.OK {
				return &driver.Status{Code: code, Detail: detail, Via: "tcp"}, nil
			}
		}
		return nil, &driver.Error{Code: driver.CommsError,
			Detail: "printer doesn't answer status queries (paused, head open or out of labels?): " + err.Error()}
	}
	st := &driver.Status{Code: driver.OK, Via: "tcp", Idle: h.Formats == 0 && h.LabelsRemaining == 0,
		Media: &driver.Media{ID: "unknown", Name: "labels loaded", Kind: driver.DieCut, DPI: z.model.DPI,
			LengthMM: float64(h.LabelLengthDots) / z.dpm(), PrintableLengthDots: h.LabelLengthDots}}
	if hq != nil {
		st.Code, st.Detail = hq.code()
	}
	if st.Code == driver.OK {
		st.Code, st.Detail = h.code()
	}
	return st, nil
}

// code maps the ~HS flags (all there is on firmware without ~HQES).
func (h hs) code() (driver.Code, string) {
	switch {
	case h.PaperOut:
		return driver.NoMedia, "out of labels"
	case h.HeadUp:
		return driver.CoverOpen, "print head open"
	case h.RibbonOut:
		return driver.RibbonOut, "ribbon out"
	case h.Paused:
		return driver.Paused, "printer is paused: press the feed button"
	}
	return driver.OK, ""
}

// dpm is the head's dots per mm (8 at "203 dpi", 12 at "300 dpi").
func (z *zpl) dpm() float64 { return math.Round(float64(z.model.DPI) / 25.4) }

func (z *zpl) size(job *driver.Job) (w, l int) {
	w, l = job.Media.PrintableWidthDots, job.Media.PrintableLengthDots
	if z.opts.PrintWidthDots > 0 {
		w = z.opts.PrintWidthDots
	}
	if l == 0 && len(job.Pages) > 0 { // continuous: as long as the page
		l = job.Pages[0].Bounds().Dy()
	}
	return w, l
}

func (z *zpl) Encode(w io.Writer, job *driver.Job) error {
	pw, ll := z.size(job)
	for _, pg := range job.Pages {
		if err := encodePageAt(w, pg, pw, ll, z.opts.OffsetXDots, z.opts.OffsetYDots); err != nil {
			return err
		}
	}
	return nil
}

// Print checks the loaded label length, sends every page as its own format in one go and then
// watches the print buffer: confirmed once the job was seen in it and it has emptied.
func (z *zpl) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	if st := job.Status; st != nil && st.Media != nil && st.Media.PrintableLengthDots > 0 && job.Media.Kind == driver.DieCut {
		if diff := job.Media.PrintableLengthDots - st.Media.PrintableLengthDots; math.Abs(float64(diff)) > z.dpm() {
			return nil, &driver.Error{Code: driver.MediaMismatch, Detail: fmt.Sprintf("config says %s, the printer measured %.0f mm long labels",
				job.Media.Name, st.Media.LengthMM)}
		}
	}
	var b bytes.Buffer
	if err := z.Encode(&b, job); err != nil {
		return nil, err
	}
	if err := t.Open(ctx); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	_, err := t.Write(b.Bytes())
	t.Close()
	if err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: b.Len()}
	sent := time.Now()
	deadline := sent.Add(10*time.Second + 2*time.Second*time.Duration(len(job.Pages)))
	seen := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			res.Message = "sent (not confirmed: stopped waiting)"
			return res, nil
		case <-time.After(pollEvery):
		}
		st, err := z.Status(ctx, t)
		if err != nil {
			continue // silent: maybe busy, maybe an error ~HQES can't name; keep watching
		}
		if st.Code != driver.OK {
			return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
		}
		switch {
		case !st.Idle:
			seen = true
		case seen:
			res.Confirmed, res.Message = true, "printed"
			return res, nil
		case time.Since(sent) > notSeenAfter:
			res.Message = "sent, probably printed (the printer finished before we could see it)"
			return res, nil
		}
	}
	res.Message = "sent (not confirmed: the printer is still busy)"
	return res, nil
}
