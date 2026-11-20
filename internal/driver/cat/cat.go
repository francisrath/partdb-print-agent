// Package cat drives Bluetooth LE "cat printers" (GB01/GT01/MX05–MX10 and clones) speaking the
// `51 78` command set: 384-dot rows over a write characteristic, status as notifications.
package cat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() {
	driver.Register("cat", func(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
		speed, ok := speeds[o.Darkness]
		if !ok {
			return nil, fmt.Errorf("cat: darkness %q must be light, normal or dark", o.Darkness)
		}
		return &cat{model: m, speed: speed}, nil
	})
}

// Darkness is mostly speed (a slower head prints darker; heat hardly matters past 0x4000).
var speeds = map[string]byte{"light": 0x60, "": 0x80, "normal": 0x80, "dark": 0xC0}

const energy = 0x6000

// Timings; tests shorten them.
var (
	replyWait      = 1500 * time.Millisecond // for an A3 status reply
	pollEvery      = 300 * time.Millisecond
	confirmBase    = 30 * time.Second // then "sent (not confirmed)"
	confirmPerPage = 2 * time.Second
	idleWait       = 10 * time.Second       // for a printer still busy with a previous job
	drainWait      = 300 * time.Millisecond // notifications of the previous job dropped before ours
)

// Status flags in the first byte of an A3 reply.
const (
	flagNoPaper    = 0x01
	flagCoverOpen  = 0x02
	flagOverheat   = 0x04
	flagBatteryLow = 0x08
	flagBusy       = 0x80
)

type cat struct {
	model *devicedb.Model
	speed byte
}

// reader parses frames off the notification stream, keeping partial frames between calls.
type reader struct {
	t       driver.Transport
	buf     []byte
	done    bool // a D3 frame came by
	replies int  // A3 replies taken
}

// take consumes the buffered whole frames up to the first A3 reply (reply) or all of them,
// noting D3 and skipping the rest (flow control AE).
func (r *reader) take(reply bool) (flags byte, ok bool) {
	for {
		f, rest, ok := nextFrame(r.buf)
		r.buf = rest
		if !ok {
			return 0, false
		}
		switch {
		case f.cmd == cmdDone:
			r.done = true
		case reply && f.cmd == cmdStatus && len(f.payload) > 0:
			r.replies++
			return f.payload[0], true
		}
	}
}

// status asks for the printer's state and waits for the A3 reply; replies already buffered before
// the request are stale and dropped. ErrNoReadback when none comes in time.
func (r *reader) status() (flags byte, err error) {
	r.take(false)
	if _, err := r.t.Write(frame(cmdStatus, 0x00)); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(replyWait)
	for {
		if flags, ok := r.take(true); ok {
			return flags, nil
		}
		if !time.Now().Before(deadline) {
			return 0, driver.ErrNoReadback
		}
		if err := r.read(); err != nil {
			return 0, err
		}
	}
}

// read appends what one Read brings; a timeout is no error.
func (r *reader) read() error {
	p := make([]byte, 512)
	n, err := r.t.Read(p)
	r.buf = append(r.buf, p[:n]...)
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, driver.ErrNoReadback) {
		return err
	}
	return nil
}

// drain reads for d and drops everything, D3 included (it belongs to the job before).
func (r *reader) drain(d time.Duration) error {
	for end := time.Now().Add(d); time.Now().Before(end); {
		if err := r.read(); err != nil {
			return err
		}
	}
	r.take(false)
	r.done = false
	return nil
}

func (c *cat) statusOf(flags byte) *driver.Status {
	st := &driver.Status{Code: driver.OK, Model: c.model.ID, Idle: flags&flagBusy == 0}
	switch {
	case flags&flagNoPaper != 0:
		st.Code, st.Detail = driver.NoMedia, "out of paper"
	case flags&flagCoverOpen != 0:
		st.Code, st.Detail = driver.CoverOpen, "cover open"
	case flags&flagOverheat != 0:
		st.Code, st.Detail = driver.Overheat, "overheated"
	case flags&flagBatteryLow != 0:
		st.Detail = "battery low"
	}
	return st
}

func (c *cat) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	flags, err := (&reader{t: t}).status()
	if err != nil {
		return nil, err
	}
	st := c.statusOf(flags)
	st.Via = t.Kind()
	return st, nil
}

func (c *cat) Encode(w io.Writer, job *driver.Job) error {
	feed := uint16(c.model.Quirk("feed_lines", 80))
	for _, pg := range job.Pages {
		if err := encodePage(w, pg, c.model.HeadPins, energy, c.speed, feed); err != nil {
			return err
		}
	}
	return nil
}

// Print needs a good status first (a silent cat printer is off, out of paper or covered), sends
// every page in one go (after waiting for a previous job to finish) and then polls until the
// printer has shown the job (a busy reply or D3) and is idle again.
func (c *cat) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	switch {
	case job.Status == nil:
		return nil, &driver.Error{Code: driver.CommsError, Detail: "printer sent no status reply"}
	case job.Status.Code != driver.OK:
		return nil, &driver.Error{Code: job.Status.Code, Detail: job.Status.Detail}
	}
	var b bytes.Buffer
	if err := c.Encode(&b, job); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &reader{t: t}
	if !job.Status.Idle {
		st, err := c.poll(ctx, r, time.Now().Add(idleWait), func(st *driver.Status) bool { return st.Idle })
		if err != nil {
			return nil, err
		}
		if st == nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if st == nil && r.replies == 0 {
			return nil, &driver.Error{Code: driver.CommsError, Detail: "printer stopped answering while busy with a previous job"}
		}
		if st == nil {
			return nil, &driver.Error{Code: driver.Busy, Detail: "still busy with a previous job"}
		}
		if err := r.drain(drainWait); err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
	}
	if _, err := t.Write(b.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: b.Len()}
	r.done = false // a D3 from before this job doesn't count
	seen := false
	st, err := c.poll(ctx, r, time.Now().Add(confirmBase+confirmPerPage*time.Duration(len(job.Pages))), func(st *driver.Status) bool {
		seen = seen || !st.Idle || r.done
		return st.Idle && seen
	})
	switch {
	case err != nil:
		return nil, err
	case st != nil:
		if st.Detail != "" {
			res.Warnings = append(res.Warnings, st.Detail)
		}
		res.Confirmed, res.Message = true, "printed"
	case ctx.Err() != nil:
		res.Message = "sent (not confirmed: stopped waiting)"
	case seen || r.done:
		res.Message = "sent (not confirmed: the printer never reported idle)"
	default:
		res.Message = "sent (not confirmed: printer never reported the job)"
	}
	return res, nil
}

// poll asks for the status every pollEvery until until(status) holds and returns that status;
// nil when the deadline passes or ctx ends first. A fault flag or a broken link is an error.
func (c *cat) poll(ctx context.Context, r *reader, deadline time.Time, until func(*driver.Status) bool) (*driver.Status, error) {
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(pollEvery):
		}
		flags, err := r.status()
		if errors.Is(err, driver.ErrNoReadback) {
			continue // no reply this time; keep asking
		}
		if err != nil {
			return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
		}
		st := c.statusOf(flags)
		if st.Code != driver.OK {
			return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
		}
		if until(st) {
			return st, nil
		}
	}
	return nil, nil
}
