package niimbot

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/driver"
)

func hexb(t *testing.T, s string) []byte {
	t.Helper()
	var b []byte
	for _, f := range strings.Fields(s) {
		var v byte
		for _, c := range f {
			v = v<<4 | byte(strings.IndexRune("0123456789ABCDEF", c))
		}
		b = append(b, v)
	}
	return b
}

// Golden packets from the protocol notes (checksums worked out by hand).
func TestPackets(t *testing.T) {
	for _, tc := range []struct {
		got  []byte
		want string
	}{
		{packet(cmdDensity, 3), "55 55 21 01 03 23 AA AA"},
		{packet(cmdLabelType, 1), "55 55 23 01 01 23 AA AA"},
		{packet(cmdPrintStart, 0, 1, 0, 0, 0, 0, 0), "55 55 01 07 00 01 00 00 00 00 00 07 AA AA"},
		{packet(cmdPageSize, 0x00, 0xF0, 0x01, 0x80, 0x00, 0x01), "55 55 13 06 00 F0 01 80 00 01 65 AA AA"},
		{packet(cmdEmptyRows, 0, 0, 10), "55 55 84 03 00 00 0A 8D AA AA"},
		{packet(cmdPageEnd, 1), "55 55 E3 01 01 E3 AA AA"},
		{packet(cmdHeartbeat, 4), "55 55 DC 01 04 D9 AA AA"},
		{connectPacket, "03 55 55 C1 01 01 C1 AA AA"},
	} {
		if want := hexb(t, tc.want); !bytes.Equal(tc.got, want) {
			t.Errorf("got % X, want % X", tc.got, want)
		}
	}
}

func TestNextPacket(t *testing.T) {
	good := packet(0xB3, 0, 1, 100, 100)
	bad := packet(0xB3, 0, 2)
	bad[len(bad)-3] ^= 0xFF // checksum
	stream := cat([]byte{0x00, 0x55}, bad, good, packet(0xF4, 1))
	var got []byte
	rest := stream
	for {
		p, r, ok := nextPacket(rest)
		rest = r
		if !ok {
			break
		}
		got = append(got, p.cmd)
	}
	if !bytes.Equal(got, []byte{0xB3, 0xF4}) || len(rest) != 0 {
		t.Errorf("got % X, rest % X", got, rest)
	}
	// a packet split across notifications waits for its tail
	if _, rest, ok := nextPacket(good[:5]); ok || !bytes.Equal(rest, good[:5]) {
		t.Errorf("partial: ok %v rest % X", ok, rest)
	}
}

func cat(bs ...[]byte) []byte { return bytes.Join(bs, nil) }

// page is a w×h white page with black dots where black(x, y).
func page(w, h int, black func(x, y int) bool) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.Gray{Y: 255}
			if black(x, y) {
				c.Y = 0
			}
			img.SetGray(x, y, c)
		}
	}
	return img
}

func TestRowPackets(t *testing.T) {
	// 16 dots, 3 blank rows, 2 rows with dot 0 and dot 9, then 300 rows all black (split at 255)
	img := page(16, 305, func(x, y int) bool {
		return y >= 3 && y < 5 && (x == 0 || x == 9) || y >= 5
	})
	got := rowPackets(img, 16, 96, false, false)
	want := [][]byte{
		packet(cmdEmptyRows, 0, 0, 3),
		packet(cmdBitmapRow, 0, 3, 2, 0, 0, 2, 0x80, 0x40), // a 96-dot head counts per 4 bytes: both dots in the first third
		packet(cmdBitmapRow, 0, 5, 16, 0, 0, 255, 0xFF, 0xFF),
		packet(cmdBitmapRow, 1, 4, 16, 0, 0, 45, 0xFF, 0xFF),
	}
	if len(got) != len(want) {
		t.Fatalf("%d packets, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("packet %d: % X, want % X", i, got[i], want[i])
		}
	}
	// total mode (B21): 0, count low, count high
	tot := rowPackets(page(16, 1, func(x, _ int) bool { return true }), 16, 384, true, false)
	if want := packet(cmdBitmapRow, 0, 0, 0, 16, 0, 1, 0xFF, 0xFF); !bytes.Equal(tot[0], want) {
		t.Errorf("total: % X, want % X", tot[0], want)
	}
	// split counts per 16 bytes on a 384-dot head
	split := rowPackets(page(384, 1, func(x, _ int) bool { return x == 0 || x == 130 || x == 260 || x == 383 }), 384, 384, false, false)
	if d := split[0][4:10]; !bytes.Equal(d, []byte{0, 0, 1, 1, 2, 1}) {
		t.Errorf("split header % X", d)
	}
}

// Dots past the head are cut off; rows wider than the page are padded white.
func TestRowPacketsHeadClamp(t *testing.T) {
	got := rowPackets(page(100, 1, func(x, _ int) bool { return x >= 96 }), 96, 96, false, false)
	if !bytes.Equal(got[0], packet(cmdEmptyRows, 0, 0, 1)) {
		t.Errorf("% X", got[0])
	}
	got = rowPackets(page(4, 1, func(int, int) bool { return true }), 16, 96, false, false)
	if !bytes.Equal(got[0][10:12], []byte{0xF0, 0x00}) {
		t.Errorf("% X", got[0])
	}
}

// B21: a run breaks every 200 rows and a CheckLine follows.
func TestCheckLine(t *testing.T) {
	got := rowPackets(page(8, 450, func(int, int) bool { return false }), 8, 384, true, true)
	want := [][]byte{
		packet(cmdEmptyRows, 0, 0, 200), packet(cmdCheckLine, 0, 199, 1),
		packet(cmdEmptyRows, 0, 200, 200), packet(cmdCheckLine, 1, 143, 1),
		packet(cmdEmptyRows, 1, 144, 50),
	}
	if len(got) != len(want) {
		t.Fatalf("%d packets", len(got))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("packet %d: % X, want % X", i, got[i], want[i])
		}
	}
}

func model(t *testing.T, id string) *devicedb.Model {
	t.Helper()
	m, ok := devicedb.Default().Model(id)
	if !ok {
		t.Fatalf("no %s", id)
	}
	return m
}

func TestHeartbeat(t *testing.T) {
	b1 := &niimbot{model: model(t, "B1")}
	d110 := &niimbot{model: model(t, "D110")}
	for _, tc := range []struct {
		n    *niimbot
		p    reply
		code driver.Code
		det  string
	}{
		{b1, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 1}}, driver.OK, ""},
		{b1, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 4, 0, 1}}, driver.CoverOpen, "cover open"},
		{b1, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 1, 0}}, driver.NoMedia, "no labels"},
		{b1, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1}}, driver.OK, "battery low"},
		{d110, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 80}}, driver.OK, ""}, // 1 = closed on a D110
		{d110, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 80}}, driver.CoverOpen, "cover open"},
		{b1, reply{rspHeartbeatV4, []byte{0, 0, 90, 30, 0, 1, 1, 0, 0}}, driver.NoMedia, "no labels"},
		{b1, reply{rspHeartbeatV4, []byte{0, 0, 90, 30, 1, 0, 1, 0, 0}}, driver.CoverOpen, "cover open"},
		{b1, reply{0xDD, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0}}, driver.NoMedia, "no labels"},
		{b1, reply{rspHeartbeatV4, []byte{0}}, "", ""}, // unknown layouts: nil
		{b1, reply{0xDD, []byte{0, 0, 0}}, "", ""},
	} {
		st := tc.n.heartbeat(tc.p)
		if tc.code == "" {
			if st != nil {
				t.Errorf("% X: %+v, want nil", tc.p.data, st)
			}
			continue
		}
		if st.Code != tc.code || st.Detail != tc.det {
			t.Errorf("%s % X: %s %q, want %s %q", tc.n.model.ID, tc.p.data, st.Code, st.Detail, tc.code, tc.det)
		}
	}
}

func TestLabelsLeft(t *testing.T) {
	tag := cat(make([]byte, 8), []byte{3, 'A', 'B', 'C'}, []byte{2, '1', '2'}, []byte{0, 200, 0, 50, 1})
	if n, kind, ok := rfid(tag); !ok || n != 150 || kind != 1 {
		t.Errorf("%d %d %v", n, kind, ok)
	}
	if _, _, ok := rfid([]byte{0}); ok {
		t.Error("no tag read as a tag")
	}
}

// printer is a scripted Niimbot: it answers what is written to it.
type printer struct {
	mu       sync.Mutex
	in       []byte
	written  []reply
	protocol byte
	page     int // pages ended so far
	ended    int // print ends seen
	// answer overrides the default answer to a request (nil: no answer, ok false: default)
	answer func(p reply) (out []byte, ok bool)
}

func (f *printer) Open(context.Context) error { return nil }
func (f *printer) Close() error               { return nil }
func (f *printer) Kind() string               { return "ble" }

func (f *printer) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for rest := b; ; {
		p, r, ok := nextPacket(rest)
		if !ok {
			break
		}
		rest = r
		f.written = append(f.written, p)
		if f.answer != nil {
			if out, ok := f.answer(p); ok {
				f.in = append(f.in, out...)
				continue
			}
		}
		switch p.cmd {
		case cmdEmptyRows, cmdBitmapRow:
		case cmdConnect:
			f.in = append(f.in, packet(0xC2, f.protocol)...)
		case cmdHeartbeat:
			f.in = append(f.in, packet(0xDD, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 1)...)
		case cmdCheckLine:
			f.in = append(f.in, packet(rspCheckLine, p.data...)...)
		case cmdPageEnd:
			f.page++
			f.in = append(f.in, packet(0xE4, 1)...)
		case cmdPrintStatus:
			f.in = append(f.in, packet(0xB3, 0, byte(f.page), 100, 100)...)
		case cmdPrintEnd:
			f.ended++
			f.in = append(f.in, packet(0xF4, 1)...)
		default:
			f.in = append(f.in, packet(answers[p.cmd], 1)...)
		}
	}
	return len(b), nil
}

func (f *printer) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		f.mu.Unlock()
		time.Sleep(time.Millisecond)
		f.mu.Lock()
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func (f *printer) cmds() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var c []byte
	for _, p := range f.written {
		if p.cmd != cmdEmptyRows && p.cmd != cmdBitmapRow {
			c = append(c, p.cmd)
		}
	}
	return c
}

func fast(t *testing.T) {
	vars := []*time.Duration{&replyWait, &packetGap, &pollEvery, &confirmBase, &confirmPerPage, &pageIndexWait}
	vals := []time.Duration{50 * time.Millisecond, 0, time.Millisecond, 200 * time.Millisecond, 0, 50 * time.Millisecond}
	for i, v := range vars {
		old := *v
		*v = vals[i]
		t.Cleanup(func() { *v = old })
	}
}

func job(pages int, media *devicedb.Media) *driver.Job {
	if media == nil {
		media = &devicedb.Media{ID: "niimbot-12x40", Kind: devicedb.DieCut}
	}
	j := &driver.Job{Media: media, Status: &driver.Status{Code: driver.OK}}
	for range pages {
		j.Pages = append(j.Pages, page(96, 20, func(x, y int) bool { return x == y }))
	}
	return j
}

func newDriver(t *testing.T, id, darkness string) *niimbot {
	t.Helper()
	d, err := driver.New(model(t, id), driver.Options{Darkness: darkness})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*niimbot)
}

func TestDarkness(t *testing.T) {
	for _, tc := range []struct {
		id, dark string
		want     byte
	}{{"B1", "", 3}, {"B1", "light", 2}, {"B1", "dark", 5}, {"D11", "", 2}, {"D11", "light", 1}, {"D11", "dark", 3}} {
		if d := newDriver(t, tc.id, tc.dark); d.density != tc.want {
			t.Errorf("%s %s: density %d, want %d", tc.id, tc.dark, d.density, tc.want)
		}
	}
	if _, err := driver.New(model(t, "B1"), driver.Options{Darkness: "max"}); err == nil {
		t.Error("darkness max accepted")
	}
}

// Each dialect sends its own sequence and confirms the job its own way.
func TestPrintDialects(t *testing.T) {
	fast(t)
	gap := &devicedb.Media{ID: "niimbot-12x40", Kind: devicedb.DieCut}
	for _, tc := range []struct {
		id   string
		want []byte // commands written, rows left out
	}{
		{"B1", []byte{0x21, 0x23, 0x01, 0x03, 0x13, 0xE3, 0x03, 0x13, 0xE3, 0xA3, 0xF3}},
		{"D110", []byte{0x21, 0x23, 0x01, 0x20, 0x03, 0x13, 0x15, 0xE3, 0xA3, 0x20, 0x03, 0x13, 0x15, 0xE3, 0xA3, 0xF3}},
		{"B21", []byte{0x21, 0x23, 0x01, 0x03, 0x13, 0xE3, 0x03, 0x13, 0xE3, 0xF3}},
		{"B21-PRO", []byte{0x23, 0x21, 0x01, 0xA3, 0x13, 0xE3, 0xA3, 0x13, 0xE3, 0xA3, 0xF3, 0xDC}},
	} {
		d := newDriver(t, tc.id, "")
		f := &printer{}
		res, err := d.Print(context.Background(), f, job(2, gap))
		if err != nil {
			t.Errorf("%s: %v", tc.id, err)
			continue
		}
		if !res.Confirmed || res.Message != "printed" || res.Pages != 2 {
			t.Errorf("%s: %+v", tc.id, res)
		}
		if got := f.cmds(); !bytes.Equal(got, tc.want) {
			t.Errorf("%s: sent % X, want % X", tc.id, got, tc.want)
		}
		var enc bytes.Buffer
		if err := d.Encode(&enc, job(2, gap)); err != nil || enc.Len() == 0 {
			t.Errorf("%s Encode: %d %v", tc.id, enc.Len(), err)
		}
	}
}

// D11: the job ends when the unsolicited page count reaches the pages sent.
func TestPrintD11PageIndex(t *testing.T) {
	fast(t)
	d := newDriver(t, "D11", "")
	f := &printer{}
	f.answer = func(p reply) ([]byte, bool) {
		if p.cmd == cmdPageEnd {
			f.page++
			return cat(packet(0xE4, 1), packet(rspPageIndex, 0, byte(f.page))), true
		}
		return nil, false
	}
	res, err := d.Print(context.Background(), f, job(2, nil))
	if err != nil || !res.Confirmed {
		t.Fatalf("%+v %v", res, err)
	}
	if got, want := f.cmds(), []byte{0x21, 0x23, 0x01, 0x20, 0x03, 0x13, 0x15, 0xE3, 0x20, 0x03, 0x13, 0x15, 0xE3, 0xF3}; !bytes.Equal(got, want) {
		t.Errorf("sent % X", got)
	}
	// without page counts the job is sent but not confirmed, and the print still ends
	f = &printer{}
	res, err = d.Print(context.Background(), f, job(1, nil))
	if err != nil || res.Confirmed || !strings.Contains(res.Message, "not confirmed") || f.ended != 1 {
		t.Errorf("%+v %v ended %d", res, err, f.ended)
	}
}

// B21: a print end answered 00 means still printing; it is asked again.
func TestPrintB21Polls(t *testing.T) {
	fast(t)
	f := &printer{}
	busy := 2
	f.answer = func(p reply) ([]byte, bool) {
		if p.cmd == cmdPrintEnd && busy > 0 {
			busy--
			return packet(0xF4, 0), true
		}
		return nil, false
	}
	res, err := newDriver(t, "B21", "").Print(context.Background(), f, job(1, nil))
	if err != nil || !res.Confirmed {
		t.Fatalf("%+v %v", res, err)
	}
	if n := bytes.Count(f.cmds(), []byte{cmdPrintEnd}); n != 3 {
		t.Errorf("%d print ends", n)
	}
}

// The print end that closes the job after the polling window still confirms it when it answers 01.
func TestPrintB21LastEnd(t *testing.T) {
	fast(t)
	pollEvery = 300 * time.Millisecond // longer than the window: one poll, then the closing print end
	f := &printer{}
	ends := 0
	f.answer = func(p reply) ([]byte, bool) {
		if p.cmd != cmdPrintEnd {
			return nil, false
		}
		ends++
		return packet(0xF4, byte(min(ends-1, 1))), true
	}
	res, err := newDriver(t, "B21", "").Print(context.Background(), f, job(1, nil))
	if err != nil || !res.Confirmed || ends != 2 {
		t.Fatalf("%+v %v, %d print ends", res, err, ends)
	}
}

// A D110 that never reports a page stops the job there: the rest isn't sent, the result says so.
func TestPrintD110Partial(t *testing.T) {
	fast(t)
	f := &printer{answer: func(p reply) ([]byte, bool) {
		return packet(0xB3, 0, 0, 0, 0), p.cmd == cmdPrintStatus
	}}
	res, err := newDriver(t, "D110", "").Print(context.Background(), f, job(2, nil))
	if err != nil || res.Confirmed || res.Pages != 1 || !strings.Contains(res.Message, "page 1 of 2") {
		t.Fatalf("%+v %v", res, err)
	}
	if n := bytes.Count(f.cmds(), []byte{cmdPageStart}); n != 1 {
		t.Errorf("%d pages started", n)
	}
}

func TestPrintErrors(t *testing.T) {
	fast(t)
	for _, tc := range []struct {
		name   string
		answer func(p reply) ([]byte, bool)
		code   driver.Code
	}{
		{"cover open on page start", func(p reply) ([]byte, bool) {
			return packet(rspError, 0x01), p.cmd == cmdPageStart
		}, driver.CoverOpen},
		{"no labels in the status", func(p reply) ([]byte, bool) {
			return packet(0xB3, 0, 0, 0, 0, 0, 0, 0x02, 0, 0, 0), p.cmd == cmdPrintStatus
		}, driver.NoMedia},
		{"not supported", func(p reply) ([]byte, bool) {
			return packet(rspNotSupported, 0), p.cmd == cmdPageSize
		}, driver.Unsupported},
		{"silent", func(p reply) ([]byte, bool) { return nil, p.cmd == cmdDensity }, driver.CommsError},
		{"refused", func(p reply) ([]byte, bool) { return packet(0xE4, 0), p.cmd == cmdPageEnd }, driver.Unknown},
	} {
		f := &printer{answer: tc.answer}
		_, err := newDriver(t, "B1", "").Print(context.Background(), f, job(1, nil))
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
		if f.ended == 0 { // the job is closed after an error too
			t.Errorf("%s: no print end", tc.name)
		}
	}
	// a bad status before the job sends nothing
	f := &printer{}
	j := job(1, nil)
	j.Status = &driver.Status{Code: driver.CoverOpen, Detail: "cover open"}
	if _, err := newDriver(t, "B1", "").Print(context.Background(), f, j); err == nil || len(f.written) != 0 {
		t.Errorf("%v, %d packets", err, len(f.written))
	}
}

func TestStatus(t *testing.T) {
	fast(t)
	tag := cat(make([]byte, 8), []byte{1, 'A'}, []byte{1, '1'}, []byte{0, 100, 0, 40, 1})
	f := &printer{protocol: 3}
	var hb byte
	f.answer = func(p reply) ([]byte, bool) {
		switch p.cmd {
		case cmdHeartbeat:
			hb = p.data[0]
			return packet(rspHeartbeatV4, 0, 0, 90, 30, 0, 0, 1, 0, 0), true
		case cmdRFID:
			return packet(0x1B, tag...), true
		}
		return nil, false
	}
	st, err := newDriver(t, "B21-PRO", "").Status(context.Background(), f)
	if err != nil || st.Code != driver.OK || st.Detail != "60 labels left on the roll" || st.Via != "ble" || hb != 4 {
		t.Fatalf("%+v %v (heartbeat type %d)", st, err, hb)
	}
	// a heartbeat in a layout we don't know is no ready printer
	f = &printer{protocol: 3, answer: func(p reply) ([]byte, bool) {
		return packet(rspHeartbeatV4, 0), p.cmd == cmdHeartbeat
	}}
	var de *driver.Error
	if _, err := newDriver(t, "B21-PRO", "").Status(context.Background(), f); !errors.As(err, &de) || de.Code != driver.Unknown {
		t.Errorf("short heartbeat: %v", err)
	}
	// a silent printer has no status
	f = &printer{answer: func(reply) ([]byte, bool) { return nil, true }}
	if _, err := newDriver(t, "B1", "").Status(context.Background(), f); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silent: %v", err)
	}
}

func TestModels(t *testing.T) {
	db := devicedb.Default()
	for _, tc := range []struct{ name, id string }{
		{"B1-H115121009", "B1"}, {"B21-C2B12345", "B21"}, {"B21S-1234", "B21S"}, {"D11-G1234", "D11"},
		{"D110-G326030306", "D110"}, {"D101-1", "D101"}, {"B18-1", "B18"}, {"B21_PRO-1", "B21-PRO"}, {"D11_H-1", "D11-H"},
		{"B1-PRO-123", "B1-PRO"}, {"B21-PRO-123", "B21-PRO"}, {"D11-H-123", "D11-H"}, {"B1 PRO-1", "B1-PRO"},
	} {
		m, ok := db.ModelByBLEName(tc.name)
		if !ok || m.ID != tc.id {
			t.Errorf("%s: %v %v, want %s", tc.name, m, ok, tc.id)
		}
	}
	for _, m := range db.Models("niimbot") {
		if _, err := driver.New(m, driver.Options{}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
		if m.QuirkUUID("ble_write", 0) != m.QuirkUUID("ble_notify", 0) || len(db.MediaFor(m)) == 0 {
			t.Errorf("%s: %v", m.ID, m.Quirks)
		}
		for _, md := range db.MediaFor(m) {
			if md.PrintableWidthDots > m.HeadPins || md.PrintableWidthDots == 0 {
				t.Errorf("%s %s: %d dots", m.ID, md.ID, md.PrintableWidthDots)
			}
		}
	}
}

// The roll's RFID tag says what kind of label it is; the print uses that label type (a transparent roll
// printed blank as "with gaps" on a B21S) and falls back to the media kind without a tag.
func TestLabelTypeFromRFID(t *testing.T) {
	fast(t)
	tag := cat(make([]byte, 8), []byte{1, 'A'}, []byte{1, '1'}, []byte{0, 180, 0, 37, 5})
	f := &printer{protocol: 3, answer: func(p reply) ([]byte, bool) {
		return packet(0x1B, tag...), p.cmd == cmdRFID
	}}
	d := newDriver(t, "B21S", "")
	st, err := d.Status(context.Background(), f)
	if err != nil || st.MediaType != "transparent" || st.Detail != "143 labels left on the roll" {
		t.Fatalf("%+v %v", st, err)
	}
	for _, tc := range []struct {
		status *driver.Status
		media  devicedb.MediaKind
		want   byte
	}{
		{st, devicedb.DieCut, 5},
		{&driver.Status{Code: driver.OK}, devicedb.DieCut, 1},
		{&driver.Status{Code: driver.OK}, devicedb.Continuous, 3},
		{&driver.Status{Code: driver.OK, MediaType: "brother-only"}, devicedb.DieCut, 1},
	} {
		f := &printer{}
		j := job(1, &devicedb.Media{ID: "m", Kind: tc.media})
		j.Status = tc.status
		if _, err := d.Print(context.Background(), f, j); err != nil {
			t.Fatal(err)
		}
		for _, p := range f.written {
			if p.cmd == cmdLabelType && p.data[0] != tc.want {
				t.Errorf("%q %v: label type %d, want %d", tc.status.MediaType, tc.media, p.data[0], tc.want)
			}
		}
	}
}

// A B21S (firmware 40.27) prints a blank label for the first D110-style job on a connection; the B1
// print task prints at once. It also took up to 3.5 s to answer the page end.
func TestB21SPrintsB1Style(t *testing.T) {
	fast(t)
	setDur(t, &pageEndWait, 300*time.Millisecond)
	f := &printer{}
	f.answer = func(p reply) ([]byte, bool) {
		if p.cmd != cmdPageEnd {
			return nil, false
		}
		f.page++
		go func() { // answers after replyWait (50 ms here), within pageEndWait
			time.Sleep(120 * time.Millisecond)
			f.mu.Lock()
			f.in = append(f.in, packet(0xE4, 1)...)
			f.mu.Unlock()
		}()
		return nil, true
	}
	res, err := newDriver(t, "B21S", "").Print(context.Background(), f, job(1, nil))
	if err != nil || !res.Confirmed {
		t.Fatalf("%+v %v", res, err)
	}
	if got, want := f.cmds(), []byte{0x21, 0x23, 0x01, 0x03, 0x13, 0xE3, 0xA3, 0xF3}; !bytes.Equal(got, want) {
		t.Errorf("sent % X, want % X (the B1 sequence)", got, want)
	}
}

func setDur(t *testing.T, p *time.Duration, v time.Duration) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
