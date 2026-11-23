// Package niimbot drives Niimbot label makers (B1, B21, B21S, D11, D110, D101, …) over Bluetooth LE: framed
// request/answer packets on one GATT characteristic, the label sent as rows across the head, the end of the
// job read back from the printer. Dialects are niimbluelib's print tasks (d11, d110, b1, b21, v4).
package niimbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

func init() {
	driver.Register("niimbot", func(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
		if !slices.Contains([]string{"d11", "d110", "b1", "b21", "v4"}, m.Dialect) {
			return nil, fmt.Errorf("niimbot: unknown dialect %q", m.Dialect)
		}
		// density 1..density_max; the default is the middle, as in Niimbot's own model list
		hi := m.Quirk("density_max", 3)
		mid := (hi + 1) / 2
		d, ok := map[string]int{"light": mid - 1, "": mid, "normal": mid, "dark": hi}[o.Darkness]
		if !ok {
			return nil, fmt.Errorf("niimbot: darkness %q must be light, normal or dark", o.Darkness)
		}
		return &niimbot{model: m, density: byte(max(d, 1))}, nil
	})
}

// Timings; tests shorten them.
var (
	replyWait      = 2 * time.Second       // for an answer to a request
	pageEndWait    = 10 * time.Second      // for the page end's answer: a B21S took up to 3.5 s (niimbluelib allows 10)
	packetGap      = 10 * time.Millisecond // between packets: a B1 prints nothing without it (niimblue #5)
	pollEvery      = 300 * time.Millisecond
	confirmBase    = 30 * time.Second // then "sent (not confirmed)"
	confirmPerPage = 5 * time.Second
	pageIndexWait  = 5 * time.Second // D11: for the next page count, then "not confirmed"
)

type niimbot struct {
	model   *devicedb.Model
	density byte
}

// step is one packet and the answers that may follow it (none: it isn't answered).
type step struct {
	p    []byte
	want []byte
}

// answers maps a request to its answer.
var answers = map[byte]byte{
	cmdPrintStart: 0x02, cmdPageStart: 0x04, cmdPageSize: 0x14, cmdQuantity: 0x16, cmdPrintClear: 0x30,
	cmdDensity: 0x31, cmdLabelType: 0x33, cmdRFID: 0x1B, cmdCheckLine: rspCheckLine, cmdPrintStatus: 0xB3,
	cmdConnect: 0xC2, cmdPageEnd: 0xE4, cmdPrintEnd: 0xF4,
}

func ask(cmd byte, data ...byte) step { return step{packet(cmd, data...), []byte{answers[cmd]}} }

// labelTypes are the label kinds a roll's RFID tag reports (niimbluelib's LabelType), by the name the
// status carries; the print sends the byte as its label type.
var labelTypes = map[byte]string{1: "with-gaps", 2: "black", 3: "continuous", 4: "perforated", 5: "transparent",
	6: "pvc-tag", 10: "black-mark-gap", 11: "heat-shrink-tube"}

// labelType is the roll's own kind where its tag reported one (a transparent roll prints blank as
// "with gaps"), else 1 = with gaps or 3 = continuous from the media.
func labelType(job *driver.Job) byte {
	if job.Status != nil {
		for b, name := range labelTypes {
			if name == job.Status.MediaType {
				return b
			}
		}
	}
	if job.Media != nil && job.Media.Kind == devicedb.Continuous {
		return 3
	}
	return 1
}

// steps is the job up to the last page's end; the wait for the printer and the print end follow.
func (n *niimbot) steps(job *driver.Job) []step {
	head := n.model.HeadPins
	pages := len(job.Pages)
	density, label := ask(cmdDensity, n.density), ask(cmdLabelType, labelType(job))
	var s []step
	switch n.model.Dialect {
	case "b1":
		s = []step{density, label, ask(cmdPrintStart, append(u16(pages), 0, 0, 0, 0, 0)...)}
	case "v4":
		s = []step{label, density, ask(cmdPrintStart, append(u16(pages), 0, 0, 0, 0, 0, 1, 0)...)}
	default:
		s = []step{density, label, ask(cmdPrintStart, 0x01)}
	}
	for _, pg := range job.Pages {
		r := pg.Bounds()
		rows, cols := r.Dy(), min(r.Dx(), head)
		switch n.model.Dialect {
		case "d11":
			s = append(s, ask(cmdPrintClear, 1), ask(cmdPageStart, 1), ask(cmdPageSize, u16(rows)...), ask(cmdQuantity, u16(1)...))
		case "d110":
			s = append(s, ask(cmdPrintClear, 1), ask(cmdPageStart, 1), ask(cmdPageSize, append(u16(rows), u16(cols)...)...),
				ask(cmdQuantity, u16(1)...))
		case "b1":
			s = append(s, ask(cmdPageStart, 1), ask(cmdPageSize, slices.Concat(u16(rows), u16(cols), u16(1))...))
		case "b21":
			s = append(s, ask(cmdPageStart, 1), ask(cmdPageSize, append(u16(rows), u16(cols)...)...))
		case "v4":
			// a B21 Pro drops the first packet after the print start: a status request nobody waits for (as the app does)
			s = append(s, step{p: packet(cmdPrintStatus, 1)},
				ask(cmdPageSize, slices.Concat(u16(rows), u16(cols), u16(1), u16(0), []byte{0, 0, 0}, u16(0))...))
		}
		b21 := n.model.Dialect == "b21"
		for _, p := range rowPackets(pg, cols, head, b21, b21) {
			st := step{p: p}
			if p[2] == cmdCheckLine {
				st.want = []byte{rspCheckLine}
			}
			s = append(s, st)
		}
		s = append(s, ask(cmdPageEnd, 1))
	}
	return s
}

func (n *niimbot) Encode(w io.Writer, job *driver.Job) error {
	var b bytes.Buffer
	for _, s := range n.steps(job) {
		b.Write(s.p)
	}
	b.Write(packet(cmdPrintEnd, 1))
	_, err := w.Write(b.Bytes())
	return err
}

// reader parses packets off the notification stream, keeping partial packets between reads.
type reader struct {
	t        driver.Transport
	ctx      context.Context // nil = never cancelled
	buf      []byte
	lastPage int  // the last unsolicited page count (0xE0)
	pageSeen bool // one came since the last reset
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

// next returns the next buffered packet, noting page counts on the way.
func (r *reader) next() (reply, bool) {
	p, rest, ok := nextPacket(r.buf)
	r.buf = rest
	if ok && p.cmd == rspPageIndex && len(p.data) >= 2 {
		r.lastPage, r.pageSeen = int(p.data[0])<<8|int(p.data[1]), true
	}
	return p, ok
}

// await waits up to d for an answer in want; an error answer is a *driver.Error, silence ErrNoReadback.
func (r *reader) await(d time.Duration, want []byte) (reply, error) {
	for end := time.Now().Add(d); ; {
		if r.ctx != nil && r.ctx.Err() != nil { // a cancelled job stops waiting at once
			return reply{}, r.ctx.Err()
		}
		for {
			p, ok := r.next()
			if !ok {
				break
			}
			switch {
			case slices.Contains(want, p.cmd):
				return p, nil
			case p.cmd == rspError && len(p.data) > 0:
				return p, printerError(p.data[0])
			case p.cmd == rspNotSupported:
				return p, &driver.Error{Code: driver.Unsupported, Detail: "the printer doesn't support a command we sent (wrong model or dialect?)"}
			}
		}
		if !time.Now().Before(end) {
			return reply{}, driver.ErrNoReadback
		}
		if err := r.read(); err != nil {
			return reply{}, err
		}
	}
}

// do sends one step and waits for its answer, if it has one.
func (r *reader) do(s step) (reply, error) {
	time.Sleep(packetGap)
	if _, err := r.t.Write(s.p); err != nil {
		return reply{}, err
	}
	if s.want == nil {
		return reply{}, nil
	}
	wait := replyWait
	if s.p[2] == cmdPageEnd {
		wait = pageEndWait
	}
	return r.await(wait, s.want)
}

// printerError maps an error code (0xDB, or the last field of a long print status) to a status code.
func printerError(code byte) *driver.Error {
	e := &driver.Error{Code: driver.Unknown, Detail: fmt.Sprintf("printer error 0x%02x", code)}
	switch code {
	case 0x01:
		e.Code, e.Detail = driver.CoverOpen, "cover open"
	case 0x02, 0x08, 0x1C:
		e.Code, e.Detail = driver.NoMedia, "no labels"
	case 0x03:
		e.Code, e.Detail = driver.LowBattery, "battery too low to print: charge the printer"
	case 0x07:
		e.Code, e.Detail = driver.Overheat, "overheated"
	case 0x09:
		e.Code, e.Detail = driver.Busy, "busy"
	case 0x10, 0x20, 0x21:
		e.Code, e.Detail = driver.MediaMismatch, "wrong labels for this printer"
	}
	return e
}

// Status connects (the printer answers with its protocol level), then reads the heartbeat: cover, labels,
// battery. The label roll's RFID tag, where the printer reads one, adds how many labels are left.
func (n *niimbot) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	r := &reader{t: t, ctx: ctx}
	c, err := r.do(step{connectPacket, []byte{answers[cmdConnect]}})
	if err != nil {
		return nil, err
	}
	if len(c.data) > 0 && c.data[0] == 0x5A {
		return nil, &driver.Error{Code: driver.Unknown, Detail: "printer reports a firmware error"}
	}
	kind := byte(1)
	if len(c.data) > 0 && c.data[0] >= 3 { // protocol 3 and later answer heartbeat type 4
		kind = 4
	}
	hb, err := r.do(step{packet(cmdHeartbeat, kind), []byte{0xDD, 0xDE, 0xDF, rspHeartbeatV4}})
	if err != nil {
		return nil, err
	}
	st := n.heartbeat(hb)
	if st == nil { // a layout we don't know: don't call a printer we can't read ready
		return nil, &driver.Error{Code: driver.Unknown, Detail: fmt.Sprintf("unrecognised heartbeat answer % x", packet(hb.cmd, hb.data...))}
	}
	st.Via, st.Raw = t.Kind(), fmt.Sprintf("% x", packet(hb.cmd, hb.data...))
	if st.Code == driver.OK {
		if tag, err := r.do(ask(cmdRFID, 1)); err == nil {
			if left, kind, ok := rfid(tag.data); ok {
				st.MediaType = labelTypes[kind]
				if st.Detail == "" && left >= 0 {
					st.Detail = fmt.Sprintf("%d labels left on the roll", left)
					if st.MediaType != "" {
						st.Detail += " (" + st.MediaType + ")"
					}
				}
			}
		}
	}
	return st, nil
}

// heartbeat reads cover, labels and battery from a heartbeat answer; where they sit depends on its type and
// length; nil for a layout we don't know. Some models report the cover the other way round (quirk lid_inverted).
func (n *niimbot) heartbeat(p reply) *driver.Status {
	st := &driver.Status{Code: driver.OK, Model: n.model.ID, Idle: true}
	d := p.data
	lid, paper, battery := -1, -1, -1
	switch {
	case p.cmd == rspHeartbeatV4 && len(d) >= 9:
		battery, lid, paper = int(d[2]), int(d[4]), int(d[5])
	case p.cmd != 0xDD:
	case len(d) == 10:
		lid, battery = int(d[8]), int(d[9])
	case len(d) == 13:
		lid, battery, paper = int(d[9]), int(d[10]), int(d[11])
	case len(d) == 19:
		lid, battery, paper = int(d[15]), int(d[16]), int(d[17])
	case len(d) == 20:
		paper = int(d[18])
	}
	if lid < 0 && paper < 0 && battery < 0 {
		return nil
	}
	if lid >= 0 && n.model.QuirkBool("lid_inverted") {
		lid = 1 - min(lid, 1)
	}
	if battery >= 0 && battery <= 4 {
		battery *= 25
	}
	switch {
	case lid > 0:
		st.Code, st.Detail = driver.CoverOpen, "cover open"
	case paper > 0:
		st.Code, st.Detail = driver.NoMedia, "no labels"
	case battery == 0: // the printer refuses jobs then (seen on a B21S)
		st.Code, st.Detail = driver.LowBattery, "battery empty: charge the printer"
	case battery > 0 && battery <= 25:
		st.Detail = "battery low"
	}
	return st
}

// rfid reads labels left (total − used, or -1 when the tag counts none) and the label kind (0 when it has
// none) from an RFID answer: uuid(8), barcode and serial (length-prefixed), total, used (u16 each), kind.
// ok is false without a tag (a one-byte answer) or for one too short to read.
func rfid(d []byte) (left int, kind byte, ok bool) {
	i := 8
	for range 2 {
		if i >= len(d) {
			return 0, 0, false
		}
		i += 1 + int(d[i])
	}
	if i+4 > len(d) {
		return 0, 0, false
	}
	total, used := int(d[i])<<8|int(d[i+1]), int(d[i+2])<<8|int(d[i+3])
	if i+4 < len(d) {
		kind = d[i+4]
	}
	if total == 0 {
		return -1, kind, true
	}
	return max(total-used, 0), kind, true
}

// Print needs a good status first, sends the job packet by packet (waiting for each answer), waits for the
// printer to report the last page (each page for d110), then ends the print.
func (n *niimbot) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	switch {
	case job.Status == nil:
		return nil, &driver.Error{Code: driver.CommsError, Detail: "printer sent no status reply"}
	case job.Status.Code != driver.OK:
		return nil, &driver.Error{Code: job.Status.Code, Detail: job.Status.Detail}
	}
	r := &reader{t: t, ctx: ctx}
	res := &driver.Result{Pages: len(job.Pages), Media: job.Media.ID}
	page := 0
	for _, s := range n.steps(job) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := r.do(s)
		res.Bytes += len(s.p)
		if err != nil { // the print end still goes out, so the printer doesn't hold the job open
			return n.end(r, res, false, err)
		}
		if s.want != nil && s.want[0] != rspCheckLine && len(p.data) > 0 && p.data[0] == 0 &&
			s.p[2] != cmdPrintStart && s.p[2] != cmdPageSize {
			return n.end(r, res, false, &driver.Error{Code: driver.Unknown, Detail: fmt.Sprintf("printer refused command 0x%02x", s.p[2])})
		}
		if s.p[2] == cmdPageEnd {
			page++
			if n.model.Dialect == "d110" { // each page gets its own window, counted from its last packet
				if ok, err := n.waitStatus(ctx, r, page, time.Now().Add(confirmBase+confirmPerPage)); err != nil || !ok {
					if err == nil && page < len(job.Pages) {
						res.Pages = page
						res.Message = fmt.Sprintf("stopped after page %d of %d: the printer never reported page %d printed", page, len(job.Pages), page)
					}
					return n.end(r, res, ok, err)
				}
			}
		}
	}
	// the confirmation window starts once everything is sent
	deadline := time.Now().Add(confirmBase + confirmPerPage*time.Duration(len(job.Pages)))
	var ok bool
	var err error
	switch n.model.Dialect {
	case "d110":
		ok = true
	case "b1", "v4":
		ok, err = n.waitStatus(ctx, r, page, deadline)
	case "d11":
		ok, err = waitPageIndex(ctx, r, page)
	case "b21": // a print end answered 01 means done
		for !ok && err == nil && ctx.Err() == nil && time.Now().Before(deadline) {
			var p reply
			if p, err = r.do(ask(cmdPrintEnd, 1)); errors.Is(err, driver.ErrNoReadback) {
				err = nil
			}
			res.Bytes += 9
			ok = err == nil && len(p.data) > 0 && p.data[0] == 1
			if !ok {
				time.Sleep(pollEvery)
			}
		}
		if ok {
			res.Confirmed, res.Message = true, "printed"
			return res, nil
		}
	}
	return n.end(r, res, ok, err)
}

// end sends the print end (after an error too, so the printer doesn't wait for more) and words the result.
func (n *niimbot) end(r *reader, res *driver.Result, ok bool, err error) (*driver.Result, error) {
	pe, endErr := r.do(ask(cmdPrintEnd, 1))
	res.Bytes += 9
	if n.model.Dialect == "b21" && err == nil && endErr == nil && len(pe.data) > 0 && pe.data[0] == 1 {
		ok = true // on the B21 a print end answered 01 means done, this last one too
	}
	if n.model.Dialect == "v4" { // and the packet after the print end is dropped too
		_, _ = r.do(step{p: packet(cmdHeartbeat, 1)})
		res.Bytes += 9
	}
	switch {
	case err != nil:
		return nil, fail(err)
	case ok:
		res.Confirmed, res.Message = true, "printed"
	case endErr != nil && !errors.Is(endErr, driver.ErrNoReadback):
		return nil, fail(endErr)
	case res.Message != "":
	default:
		res.Message = "sent (not confirmed: the printer never reported the last page)"
	}
	return res, nil
}

// fail keeps a printer's error and words a broken link as a comms error.
func fail(err error) error {
	var de *driver.Error
	switch {
	case errors.As(err, &de):
		return de
	case errors.Is(err, driver.ErrNoReadback):
		return &driver.Error{Code: driver.CommsError, Detail: "printer stopped answering", Err: err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return &driver.Error{Code: driver.CommsError, Detail: err.Error(), Err: err}
}

// waitStatus polls the print status until it counts page pages; false when the deadline passes or ctx ends.
func (n *niimbot) waitStatus(ctx context.Context, r *reader, page int, deadline time.Time) (bool, error) {
	for ctx.Err() == nil && time.Now().Before(deadline) {
		p, err := r.do(ask(cmdPrintStatus, 1))
		switch {
		case errors.Is(err, driver.ErrNoReadback):
		case err != nil:
			return false, err
		case len(p.data) >= 10 && p.data[6] != 0:
			return false, printerError(p.data[6])
		case len(p.data) >= 2 && int(p.data[0])<<8|int(p.data[1]) >= page:
			return true, nil
		}
		time.Sleep(pollEvery)
	}
	return false, nil
}

// waitPageIndex waits for the D11's unsolicited page count to reach page; each count restarts the wait.
func waitPageIndex(ctx context.Context, r *reader, page int) (bool, error) {
	for end := time.Now().Add(pageIndexWait); ctx.Err() == nil && time.Now().Before(end); {
		r.pageSeen = false
		for _, ok := r.next(); ok; _, ok = r.next() {
		}
		if r.lastPage >= page {
			return true, nil
		}
		if r.pageSeen {
			end = time.Now().Add(pageIndexWait)
		}
		if err := r.read(); err != nil {
			return false, err
		}
	}
	return r.lastPage >= page, nil
}
