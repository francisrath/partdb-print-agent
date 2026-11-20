package cat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// fake is a BLE transport stand-in: every A3 status request written pops the next scripted reply
// (raw notification bytes; nil = silence) into the read stream.
type fake struct {
	mu       sync.Mutex
	replies  [][]byte
	in       []byte
	written  bytes.Buffer
	writeErr error  // returned by the first Write longer than a status request
	readErr  error  // returned by Read once the replies are used up
	perRead  int    // most bytes one Read returns (0 = any)
	onJob    []byte // notifications the job write brings (flow control during the write)
	// late arrives unsolicited lateDelay after status request number lateAfter (1-based) was written
	// (lateAfter -1: at lateAt, set by the test).
	late       []byte
	lateAfter  int
	lateDelay  time.Duration
	lateAt     time.Time
	requests   int
	lateBefore bool // late had arrived when the job was written
}

var statusRequest = frame(0xA3, 0x00)

func (f *fake) Open(context.Context) error { return nil }
func (f *fake) Close() error               { return nil }
func (f *fake) Kind() string               { return "ble" }

func (f *fake) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil && len(p) > len(statusRequest) {
		return 0, f.writeErr
	}
	f.written.Write(p)
	if !bytes.Equal(p, statusRequest) {
		f.lateBefore = f.late == nil && f.lateAfter != 0
		f.in = append(f.in, f.onJob...)
	}
	if bytes.Equal(p, statusRequest) {
		f.requests++
		if f.requests == f.lateAfter {
			f.lateAt = time.Now().Add(f.lateDelay)
		}
	}
	if bytes.Equal(p, statusRequest) && len(f.replies) > 0 {
		f.in = append(f.in, f.replies[0]...)
		f.replies = f.replies[1:]
	}
	return len(p), nil
}

func (f *fake) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.late != nil && !f.lateAt.IsZero() && time.Now().After(f.lateAt) {
		f.in, f.late = append(f.in, f.late...), nil
	}
	if len(f.in) == 0 {
		if f.readErr != nil && len(f.replies) == 0 {
			return 0, f.readErr
		}
		time.Sleep(time.Millisecond)
		return 0, os.ErrDeadlineExceeded
	}
	if f.perRead > 0 && len(p) > f.perRead {
		p = p[:f.perRead]
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func status(flags byte) []byte { return frame(0xA3, flags, 0x01, 0x95) }

// emptied is the MX10's unsolicited "buffer empty" notification, as captured (note the 01 in byte 3).
var emptied = []byte{0x51, 0x78, 0xAE, 0x01, 0x01, 0x00, 0x00, 0x00, 0xFF}

func fastTimings(t *testing.T) {
	vars := []*time.Duration{&replyWait, &pollEvery, &confirmBase, &confirmPerPage, &idleWait, &drainWait}
	fast := []time.Duration{50 * time.Millisecond, time.Millisecond, 300 * time.Millisecond, 0, 200 * time.Millisecond, 20 * time.Millisecond}
	for i, v := range vars {
		old := *v
		*v = fast[i]
		t.Cleanup(func() { *v = old })
	}
}

func mx10(t *testing.T, darkness string) (driver.Driver, *driver.Media) {
	t.Helper()
	m, ok := devicedb.Default().Model("MX10")
	if !ok {
		t.Fatal("no MX10")
	}
	d, err := driver.New(m, driver.Options{Darkness: darkness})
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range devicedb.Default().MediaFor(m) {
		if md.ID == "paper-57" {
			return d, &md
		}
	}
	t.Fatal("no paper-57")
	return nil, nil
}

func TestStatusFlags(t *testing.T) {
	fastTimings(t)
	for _, tc := range []struct {
		name   string
		reply  []byte
		code   driver.Code
		idle   bool
		detail string
	}{
		{"ok", status(0x00), driver.OK, true, ""},
		{"busy", status(0x80), driver.OK, false, ""},
		{"no paper", status(0x01), driver.NoMedia, true, "out of paper"},
		{"cover open", status(0x02), driver.CoverOpen, true, "cover open"},
		{"overheat", status(0x04), driver.Overheat, true, "overheated"},
		{"battery low", status(0x08), driver.OK, true, "battery low"},
		{"flow control first", append(frame(0xAE, 0x10), status(0x00)...), driver.OK, true, ""},
		{"D3 first", append(frame(0xD3, 0x19), status(0x01)...), driver.NoMedia, true, "out of paper"},
	} {
		d, _ := mx10(t, "")
		st, err := d.Status(context.Background(), &fake{replies: [][]byte{tc.reply}})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if st.Code != tc.code || st.Idle != tc.idle || st.Detail != tc.detail || st.Via != "ble" || st.Model != "MX10" {
			t.Errorf("%s: got %+v", tc.name, st)
		}
		if raw := tc.reply[len(tc.reply)-len(status(0)):]; st.Raw != fmt.Sprintf("% x", raw) { // the A3 reply as received
			t.Errorf("%s: raw %q, want % x", tc.name, st.Raw, raw)
		}
	}
}

func TestStatusSilent(t *testing.T) {
	fastTimings(t)
	d, _ := mx10(t, "")
	if _, err := d.Status(context.Background(), &fake{}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silent printer: err = %v, want ErrNoReadback", err)
	}
}

func job(m *driver.Media, pages int) *driver.Job {
	j := &driver.Job{Media: m, Status: &driver.Status{Code: driver.OK, Idle: true}}
	for range pages {
		j.Pages = append(j.Pages, page(384, "####", "#.#."))
	}
	return j
}

func TestPrintConfirms(t *testing.T) {
	fastTimings(t)
	d, m := mx10(t, "dark")
	f := &fake{replies: [][]byte{nil, status(0x80), status(0x00), append(append([]byte{}, emptied...), status(0x00)...)}}
	res, err := d.Print(context.Background(), f, job(m, 2))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || res.Pages != 2 || res.Message != "printed" {
		t.Errorf("result %+v", res)
	}
	var enc bytes.Buffer
	if err := d.Encode(&enc, job(m, 2)); err != nil {
		t.Fatal(err)
	}
	if res.Bytes != enc.Len() || !bytes.HasPrefix(f.written.Bytes(), enc.Bytes()) {
		t.Errorf("Print sent other bytes than Encode (%d vs %d)", res.Bytes, enc.Len())
	}
	if !bytes.Contains(enc.Bytes(), frame(0xBD, 0xC0)) || bytes.Count(enc.Bytes(), frame(0xA1, 80, 0)) != 2 {
		t.Errorf("dark speed 0xC0 or two 80-line feeds missing")
	}
}

func TestPrintNotConfirmed(t *testing.T) {
	fastTimings(t)
	d, m := mx10(t, "")
	res, err := d.Print(context.Background(), &fake{}, job(m, 1)) // silent while printing
	if err != nil {
		t.Fatal(err)
	}
	if res.Confirmed || !strings.HasPrefix(res.Message, "sent (not confirmed") {
		t.Errorf("result %+v", res)
	}
}

func TestPrintOverheat(t *testing.T) {
	fastTimings(t)
	d, m := mx10(t, "")
	_, err := d.Print(context.Background(), &fake{replies: [][]byte{status(0x80), status(0x84)}}, job(m, 1))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.Overheat {
		t.Errorf("err = %v, want OVERHEAT", err)
	}
}

func TestPrintNeedsStatus(t *testing.T) {
	fastTimings(t)
	d, m := mx10(t, "")
	for _, st := range []*driver.Status{nil, {Code: driver.NoMedia, Detail: "out of paper"}} {
		j := job(m, 1)
		j.Status = st
		f := &fake{}
		_, err := d.Print(context.Background(), f, j)
		var de *driver.Error
		if !errors.As(err, &de) || f.written.Len() != 0 {
			t.Errorf("status %+v: err = %v, %d bytes sent", st, err, f.written.Len())
		}
	}
}

func TestPrintBatteryLowWarns(t *testing.T) {
	fastTimings(t)
	d, m := mx10(t, "")
	res, err := d.Print(context.Background(), &fake{replies: [][]byte{status(0x80), append(append([]byte{}, emptied...), status(0x08)...)}}, job(m, 1))
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0] != "battery low" {
		t.Errorf("res %+v, err %v", res, err)
	}
}

func TestPrintPeerGone(t *testing.T) {
	fastTimings(t)
	gone := errors.New("peer disconnected")
	for _, tc := range []struct {
		name string
		f    *fake
	}{
		{"write", &fake{writeErr: gone}},
		{"read", &fake{replies: [][]byte{status(0x80)}, readErr: gone}},
	} {
		d, m := mx10(t, "")
		_, err := d.Print(context.Background(), tc.f, job(m, 1))
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != driver.CommsError || !strings.Contains(de.Detail, "peer disconnected") {
			t.Errorf("%s: err = %v, want COMMS_ERROR", tc.name, err)
		}
	}
}

func TestPrintNeedsJobEvidence(t *testing.T) {
	fastTimings(t)
	d3 := frame(0xD3, 0x19)
	then := func(a, b []byte) []byte { return append(append([]byte{}, a...), b...) }
	const (
		never   = "sent (not confirmed: printer never reported the job)"
		notDone = "sent (not confirmed: the printer never reported the end of the job)"
	)
	for _, tc := range []struct {
		name      string
		replies   [][]byte
		onJob     []byte
		confirmed bool
		message   string
		left      int // replies not asked for: confirmation came at the AE 00, not later
	}{
		{"idle only", [][]byte{status(0x00), status(0x00)}, nil, false, never, 0},
		// measured on an MX10: busy … idle … then AE 00 ("buffer empty") when the paper stops
		{"busy, idle, AE 00", [][]byte{status(0x80), status(0x80), status(0x00), status(0x00), then(emptied, status(0x00)), status(0x00)}, nil, true, "printed", 1},
		{"busy, idle", [][]byte{status(0x80), status(0x00), status(0x00)}, nil, false, notDone, 0},
		{"D3 no longer counts", [][]byte{status(0x80), then(d3, status(0x00)), status(0x00)}, nil, false, notDone, 0},
		{"AE 00 during the write", [][]byte{status(0x80), status(0x00)}, then(frame(0xAE, 0x10), emptied), false, notDone, 0},
		{"AE 00 before busy", [][]byte{then(emptied, status(0x80)), status(0x00)}, nil, false, notDone, 0},
		{"AE 10 after busy", [][]byte{status(0x80), then(frame(0xAE, 0x10), status(0x00))}, nil, false, notDone, 0},
		// the tail of the write paused after Write returned: its resume AE 00 is not the end of the job
		{"resume after busy", [][]byte{then(status(0x80), frame(0xAE, 0x10)), then(emptied, status(0x80)), status(0x00), status(0x00)}, nil, false, notDone, 0},
		{"resume, then buffer empty", [][]byte{then(status(0x80), frame(0xAE, 0x10)), then(emptied, status(0x80)), status(0x00), then(emptied, status(0x00)), status(0x00)}, nil, true, "printed", 1},
		// a stale idle reply already buffered behind the busy one must not answer the next request
		{"stale idle", [][]byte{append(status(0x80), status(0x00)...)}, nil, false, notDone, 0},
	} {
		d, m := mx10(t, "")
		f := &fake{replies: tc.replies, onJob: tc.onJob}
		res, err := d.Print(context.Background(), f, job(m, 1))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.Confirmed != tc.confirmed || res.Message != tc.message {
			t.Errorf("%s: result %+v", tc.name, res)
		}
		if tc.confirmed && len(f.replies) != tc.left {
			t.Errorf("%s: %d replies left, want %d", tc.name, len(f.replies), tc.left)
		}
	}
}

func TestPrintWaitsForIdle(t *testing.T) {
	fastTimings(t)
	busy := func(m *driver.Media) *driver.Job {
		j := job(m, 1)
		j.Status.Idle = false
		return j
	}
	d, m := mx10(t, "")
	f := &fake{replies: [][]byte{status(0x80), status(0x00), status(0x80), append(append([]byte{}, emptied...), status(0x00)...)}}
	if res, err := d.Print(context.Background(), f, busy(m)); err != nil || !res.Confirmed {
		t.Errorf("busy, then idle: %+v, %v", res, err)
	}
	var replies [][]byte
	for range 1000 {
		replies = append(replies, status(0x80))
	}
	f = &fake{replies: replies}
	_, err := d.Print(context.Background(), f, busy(m))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.Busy {
		t.Errorf("still busy: err = %v, want BUSY", err)
	}
	if n := bytes.Count(f.written.Bytes(), statusRequest); n*len(statusRequest) != f.written.Len() {
		t.Errorf("job bytes sent to a busy printer")
	}
	f = &fake{} // never answers while we wait: not busy, gone
	_, err = d.Print(context.Background(), f, busy(m))
	if !errors.As(err, &de) || de.Code != driver.CommsError || f.written.Len() != bytes.Count(f.written.Bytes(), statusRequest)*len(statusRequest) {
		t.Errorf("silent while waiting: err = %v, want COMMS_ERROR and no job sent", err)
	}
	// the previous job's AE 00 comes a while after its idle reply: wait for it before writing ours, so it
	// can't land after our busy reply and confirm our job
	drainWait = time.Second
	f = &fake{replies: [][]byte{status(0x80), status(0x00), status(0x80), status(0x00), status(0x00)},
		late: emptied, lateAfter: 2, lateDelay: 100 * time.Millisecond}
	start := time.Now()
	res, err := d.Print(context.Background(), f, busy(m))
	if err != nil || res.Confirmed || !f.lateBefore {
		t.Errorf("late AE 00 of the previous job: %+v, %v, arrived before our write: %v", res, err, f.lateBefore)
	}
	if time.Since(start) >= drainWait {
		t.Errorf("waited the whole drain although the AE 00 came")
	}
}

func TestDarknessSpeed(t *testing.T) {
	for darkness, speed := range map[string]byte{"light": 0x30, "": 0x80, "normal": 0x80, "dark": 0xC0} {
		d, m := mx10(t, darkness)
		var b bytes.Buffer
		if err := d.Encode(&b, job(m, 1)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b.Bytes(), frame(0xBD, speed)) {
			t.Errorf("darkness %q: no speed %#x", darkness, speed)
		}
	}
}

func TestPrintAfterUnconfirmedJob(t *testing.T) {
	fastTimings(t)
	drainWait = time.Second
	d, m := mx10(t, "")
	// the printer says idle, but the last job wasn't confirmed: its AE 00 may still come and must not
	// confirm this one
	j := job(m, 1)
	j.PrevUnconfirmed = true
	f := &fake{replies: [][]byte{status(0x80), status(0x00), status(0x00)}, late: emptied, lateAfter: -1}
	f.lateAt = time.Now().Add(100 * time.Millisecond)
	res, err := d.Print(context.Background(), f, j)
	if err != nil || res.Confirmed || !f.lateBefore {
		t.Errorf("late AE 00 of an unconfirmed job: %+v, %v, arrived before our write: %v", res, err, f.lateBefore)
	}
}
