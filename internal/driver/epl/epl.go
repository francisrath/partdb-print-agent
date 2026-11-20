// Package epl drives Zebra/Eltron printers that speak only EPL2 (LP/TLP 2824, 2844, 2742 …): every
// page is one GW graphic. Clean-room from Zebra's EPL Programming Guide 14245L-003 Rev. A. Status
// (^ee) is documented on USB and serial only; over TCP the printer is driven blind.
package epl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

func init() {
	driver.Register("epl", func(m *devicedb.Model, o driver.Options) (driver.Driver, error) {
		return &epl{model: m, opts: o}, nil
	})
}

// Completion polling; tests shorten them.
var (
	pollEvery    = 250 * time.Millisecond
	notSeenAfter = 2 * time.Second // never seen busy by then: "probably printed"
)

type epl struct {
	model *devicedb.Model
	opts  driver.Options
}

// dpm is the head's dots per mm (8 at "203 dpi", 12 at "300 dpi").
func (e *epl) dpm() float64 { return math.Round(float64(e.model.DPI) / 25.4) }

// density is D0–D15: the model's documented default (quirk density, else the manual's 7), ±3 for light/dark.
func (e *epl) density() int {
	d := e.model.Quirk("density", 7)
	switch e.opts.Darkness {
	case "light":
		d -= 3
	case "dark":
		d += 3
	}
	return min(max(d, 0), 15)
}

// Encode writes the setup once (q, Q, D and Z are stored on the printer, so never rely on what was
// set before; configuration goes before N) and then N, GW, P1 per page.
func (e *epl) Encode(w io.Writer, job *driver.Job) error {
	width := job.Media.PrintableWidthDots
	if e.model.HeadPins > 0 {
		width = min(width, e.model.HeadPins)
	}
	length := job.Media.PrintableLengthDots
	if length == 0 && len(job.Pages) > 0 { // continuous: as long as the page
		length = job.Pages[0].Bounds().Dy()
	}
	var form string
	switch {
	case job.Media.Kind == devicedb.Continuous:
		form = fmt.Sprintf("Q%d,0", length)
	case e.opts.BlackMarkMM > 0: // p2 = B + line thickness, p3 required in this mode (p.139)
		form = fmt.Sprintf("Q%d,B%d+0", length, min(max(int(math.Round(e.opts.BlackMarkMM*e.dpm())), 16), 240))
	default:
		// ponytail: a fixed 3 mm gap; the sensor finds the real one. Add a gap_mm setting if a roll needs it.
		form = fmt.Sprintf("Q%d,%d", length, int(3*e.dpm()))
	}
	// LF first clears a half-sent command line (N, p.122); ZT prints the buffer's top row first
	if _, err := fmt.Fprintf(w, "\nZT\nq%d\n%s\nD%d\n", width, form, e.density()); err != nil {
		return err
	}
	for _, pg := range job.Pages {
		if err := encodePage(w, pg, width, length); err != nil {
			return err
		}
	}
	return nil
}

// encodePage writes N, the page as one GW graphic (clipped to width × length, padded white) and P1.
// GW bits: most significant first, 0 = black dot.
func encodePage(w io.Writer, img *image.Gray, width, length int) error {
	rowBytes := (width + 7) / 8
	raw := bytes.Repeat([]byte{0xFF}, rowBytes*length)
	b := img.Bounds()
	for y := range min(length, b.Dy()) {
		row := img.Pix[y*img.Stride:]
		for x := range min(width, b.Dx()) {
			if row[x] < 128 {
				raw[y*rowBytes+x/8] &^= 0x80 >> (x % 8)
			}
		}
	}
	if _, err := fmt.Fprintf(w, "N\nGW0,0,%d,%d,", rowBytes, length); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\nP1\n")
	return err
}

var errSilent = errors.New("no ^ee reply")

// query sends ^ee on a connection of its own and reads its 4-byte reply ("XX\r\n").
func query(ctx context.Context, t driver.Transport) (string, error) {
	if err := t.Open(ctx); err != nil {
		return "", err
	}
	defer t.Close()
	if _, err := t.Write([]byte("^ee\n")); err != nil {
		return "", err
	}
	var reply []byte
	buf := make([]byte, 16)
	for !bytes.Contains(reply, []byte("\n")) && len(reply) < 64 {
		n, err := t.Read(buf)
		reply = append(reply, buf[:n]...)
		if err != nil {
			break
		}
	}
	code := string(bytes.TrimSpace(reply))
	if len(code) != 2 || code[0] < '0' || code[0] > '9' || code[1] < '0' || code[1] > '9' {
		return "", fmt.Errorf("%w (got %q)", errSilent, reply)
	}
	return code, nil
}

// codes maps the ^ee error/status codes (p.175) that stop a job.
var codes = map[string]struct {
	code   driver.Code
	detail string
}{
	"07": {driver.NoMedia, "paper or ribbon empty"},
	"11": {driver.CoverOpen, "print head up (open)"},
	"12": {driver.Paused, "printer is paused: press the feed button"},
	"81": {driver.CutterJam, "cutter jammed or not installed"},
	"82": {driver.MediaMismatch, "AutoSense or sensor failure: recalibrate the printer"},
	"84": {driver.MediaMismatch, "excessive media feeding: no gap or black mark found (wrong media or sensing?)"},
}

// Status asks ^ee over USB; on other transports EPL has no documented read-back.
func (e *epl) Status(ctx context.Context, t driver.Transport) (*driver.Status, error) {
	if t.Kind() != "usb" {
		return nil, fmt.Errorf("%w: EPL printers report status only over USB or serial", driver.ErrNoReadback)
	}
	code, err := query(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", driver.ErrNoReadback, err)
	}
	st := &driver.Status{Code: driver.OK, Via: "usb", Idle: code != "50", Raw: code}
	switch c, known := codes[code]; {
	case known:
		st.Code, st.Detail = c.code, c.detail
	case code == "00", code == "50":
	default: // 01–10: an earlier job's error (syntax, memory …); it doesn't stop the printer
		st.Detail = "last job reported EPL error " + code
	}
	return st, nil
}

// Print sends the job in one go and, over USB, watches ^ee: confirmed once it was seen busy (50)
// and is back at 00.
func (e *epl) Print(ctx context.Context, t driver.Transport, job *driver.Job) (*driver.Result, error) {
	var b bytes.Buffer
	if err := e.Encode(&b, job); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil { // stopped while rendering: send nothing
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
	if t.Kind() != "usb" {
		res.Message = "sent (not confirmed: EPL printers report status only over USB)"
		return res, nil
	}
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
		st, err := e.Status(ctx, t)
		if err != nil {
			continue // silent: keep watching
		}
		if st.Code != driver.OK {
			return nil, &driver.Error{Code: st.Code, Detail: st.Detail}
		}
		if st.Detail != "" && (job.Status == nil || job.Status.Raw != st.Raw) { // a new job error: ours
			return nil, &driver.Error{Code: driver.Unknown, Detail: st.Detail}
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
