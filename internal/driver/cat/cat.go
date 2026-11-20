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
	t   driver.Transport
	buf []byte
}

// status asks for the printer's state and waits for the A3 reply, skipping every other frame
// (flow control AE, the D3 seen after a job). ErrNoReadback when none comes in time.
func (r *reader) status() (flags byte, err error) {
	if _, err := r.t.Write(frame(cmdStatus, 0x00)); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(replyWait)
	p := make([]byte, 512)
	for {
		for {
			f, rest, ok := nextFrame(r.buf)
			if !ok {
				r.buf = rest
				break
			}
			r.buf = rest
			if f.cmd == cmdStatus && len(f.payload) > 0 {
				return f.payload[0], nil
			}
		}
		if !time.Now().Before(deadline) {
			return 0, driver.ErrNoReadback
		}
		n, err := r.t.Read(p)
		r.buf = append(r.buf, p[:n]...)
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, driver.ErrNoReadback) {
			return 0, err
		}
	}
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
		if err := encodePage(w, pg, energy, c.speed, feed); err != nil {
			return err
		}
	}
	return nil
}

// Print needs a good status first (a silent cat printer is off, out of paper or covered), sends
// every page in one go and then polls until the printer is no longer busy.
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
	if _, err := t.Write(b.Bytes()); err != nil {
		return nil, &driver.Error{Code: driver.CommsError, Detail: err.Error()}
	}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID, Bytes: b.Len()}
	r := &reader{t: t}
	deadline := time.Now().Add(confirmBase + confirmPerPage*time.Duration(len(job.Pages)))
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			res.Message = "sent (not confirmed: stopped waiting)"
			return res, nil
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
		if st.Idle {
			if st.Detail != "" {
				res.Warnings = append(res.Warnings, st.Detail)
			}
			res.Confirmed, res.Message = true, "printed"
			return res, nil
		}
	}
	res.Message = "sent (not confirmed: the printer never reported idle)"
	return res, nil
}
