package cat

import (
	"bytes"
	"context"
	"errors"
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
	writeErr error // returned by the first Write longer than a status request
	readErr  error // returned by Read once the replies are used up
	perRead  int   // most bytes one Read returns (0 = any)
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
	if bytes.Equal(p, statusRequest) && len(f.replies) > 0 {
		f.in = append(f.in, f.replies[0]...)
		f.replies = f.replies[1:]
	}
	return len(p), nil
}

func (f *fake) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f := &fake{replies: [][]byte{nil, status(0x80), append(frame(0xAE, 0x00), status(0x80)...), status(0x00)}}
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
	res, err := d.Print(context.Background(), &fake{replies: [][]byte{status(0x80), status(0x08)}}, job(m, 1))
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
	for _, tc := range []struct {
		name      string
		replies   [][]byte
		confirmed bool
		message   string
	}{
		{"idle only", [][]byte{status(0x00), status(0x00)}, false, "sent (not confirmed: printer never reported the job)"},
		{"idle, D3, idle", [][]byte{status(0x00), d3, status(0x00)}, true, "printed"},
		{"busy, idle", [][]byte{status(0x80), status(0x00)}, true, "printed"},
		// a stale idle reply already buffered behind the busy one must not answer the next request
		{"stale idle", [][]byte{append(status(0x80), status(0x00)...)}, false, "sent (not confirmed: the printer never reported idle)"},
	} {
		d, m := mx10(t, "")
		res, err := d.Print(context.Background(), &fake{replies: tc.replies}, job(m, 1))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.Confirmed != tc.confirmed || res.Message != tc.message {
			t.Errorf("%s: result %+v", tc.name, res)
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
	f := &fake{replies: [][]byte{status(0x80), status(0x00), status(0x80), status(0x00)}}
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
	// the previous job's D3 arrives just after its idle reply, before our Write: no evidence for ours
	f = &fake{replies: [][]byte{append(status(0x00), frame(0xD3, 0x19)...), status(0x00), status(0x00)}, perRead: len(status(0x00))}
	res, err := d.Print(context.Background(), f, busy(m))
	if err != nil || res.Confirmed || res.Message != "sent (not confirmed: printer never reported the job)" {
		t.Errorf("late D3 of the previous job: %+v, %v", res, err)
	}
}
