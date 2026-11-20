package escpos

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
)

// fake answers like a printer: every DLE EOT / GS r request written pops the next scripted reply
// (nil = silence) into the read stream; stale bytes can be preloaded. Reads time out when empty.
type fake struct {
	replies [][]byte
	in      []byte
	written [][]byte
	readErr error           // returned by Read instead of a timeout once the stream is empty
	zero    bool            // Read returns (0, nil) when empty
	onGSR   func()          // called when GS r is written
	timeout []time.Duration // SetReadTimeout calls
}

func (f *fake) SetReadTimeout(d time.Duration) { f.timeout = append(f.timeout, d) }

func (f *fake) Open(context.Context) error { return nil }
func (f *fake) Close() error               { return nil }
func (f *fake) Kind() string               { return "bt" }
func (f *fake) Write(p []byte) (int, error) {
	f.written = append(f.written, append([]byte(nil), p...))
	if f.onGSR != nil && bytes.Equal(p, []byte{0x1D, 0x72, 0x01}) {
		f.onGSR()
	}
	if req := len(p) == 3 && (p[0] == 0x10 || p[0] == 0x1D && p[1] == 0x72); req && len(f.replies) > 0 {
		f.in = append(f.in, f.replies[0]...)
		f.replies = f.replies[1:]
	}
	return len(p), nil
}
func (f *fake) Read(p []byte) (int, error) {
	if len(f.in) == 0 {
		time.Sleep(time.Millisecond)
		if f.readErr != nil {
			return 0, f.readErr
		}
		if f.zero {
			return 0, nil
		}
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func newDriver(t *testing.T, cutter bool) *escpos {
	t.Helper()
	m, ok := devicedb.Default().Model("RG-MLP80B")
	if !ok {
		t.Fatal("no RG-MLP80B")
	}
	c := *m
	c.Quirks = map[string]any{"feed_lines": 80, "confirm_timeout_ms": 20000}
	if cutter {
		c.Quirks["cutter"] = true
	}
	d, err := driver.New(&c, driver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := d.(*escpos)
	e.confirm = 30 * time.Millisecond
	return e
}

func img(w, h int, black func(x, y int) bool) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(255)
			if black(x, y) {
				v = 0
			}
			g.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return g
}

func encode(t *testing.T, d *escpos, pages ...*image.Gray) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := d.Encode(&b, &driver.Job{Pages: pages}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestEncodeGolden(t *testing.T) {
	// 16x3: row 0 all black, row 1 left byte black, row 2 white
	g := img(16, 3, func(x, y int) bool { return y == 0 || y == 1 && x < 8 })
	want := []byte{0x1B, 0x40, 0x1D, 0x76, 0x30, 0x00, 0x02, 0x00, 0x03, 0x00,
		0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x1B, 0x64, 0x50}
	if got := encode(t, newDriver(t, false), g); !bytes.Equal(got, want) {
		t.Errorf("got % x\nwant % x", got, want)
	}
}

func TestEncodeBandsPaddingCutter(t *testing.T) {
	got := encode(t, newDriver(t, true), img(10, 600, func(x, y int) bool { return x == 9 }))
	var heads [][]byte
	for i := 0; i+8 <= len(got); i++ {
		if bytes.Equal(got[i:i+4], []byte{0x1D, 0x76, 0x30, 0x00}) {
			heads = append(heads, got[i+4:i+8])
		}
	}
	wantHeads := [][]byte{{2, 0, 0, 1}, {2, 0, 0, 1}, {2, 0, 88, 0}} // 256, 256, 88 lines; 2 bytes/row
	if len(heads) != 3 {
		t.Fatalf("%d bands", len(heads))
	}
	for i := range heads {
		if !bytes.Equal(heads[i], wantHeads[i]) {
			t.Errorf("band %d header % x, want % x", i, heads[i], wantHeads[i])
		}
	}
	// x=9 is bit 6 of the second byte (padding bits stay 0)
	if got[10] != 0x00 || got[11] != 0x40 {
		t.Errorf("row bytes % x", got[10:12])
	}
	if want := []byte{0x1B, 0x64, 0x50, 0x1D, 0x56, 0x01}; !bytes.HasSuffix(got, want) {
		t.Errorf("tail % x, want % x", got[len(got)-6:], want)
	}
}

func TestFeedOver255(t *testing.T) {
	d := newDriver(t, false)
	d.model.Quirks["feed_lines"] = 300
	got := encode(t, d, img(8, 1, func(int, int) bool { return false }))
	if want := []byte{0x1B, 0x64, 0xFF, 0x1B, 0x64, 0x2D}; !bytes.HasSuffix(got, want) {
		t.Errorf("tail % x", got)
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads [][]byte
		code  driver.Code
		det   string
	}{
		{"ok", [][]byte{{0x12}, {0x12}, {0x12}}, driver.OK, ""},
		{"cover open", [][]byte{{0x12}, {0x16}, {0x12}}, driver.CoverOpen, "cover open"},
		{"paper end", [][]byte{{0x12}, {0x12}, {0x72}}, driver.NoMedia, "out of paper"},
		{"error", [][]byte{{0x12}, {0x52}, {0x12}}, driver.Unknown, "printer error"},
		{"near end is only a warning", [][]byte{{0x12}, {0x12}, {0x1E}}, driver.OK, "paper near end"},
		// an unsolicited ASB frame (cover open, offline) ahead of the reply is not the reply
		{"ASB frame skipped", [][]byte{{0x38, 0x00, 0x00, 0x00, 0x12}, {0x12}, {0x12}}, driver.OK, ""},
	} {
		f := &fake{replies: tc.reads}
		st, err := newDriver(t, false).Status(context.Background(), f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if st.Code != tc.code || st.Detail != tc.det || st.Via != "bt" || !st.Idle {
			t.Errorf("%s: got %+v", tc.name, st)
		}
		if len(f.written) != 3 || !bytes.Equal(f.written[2], []byte{0x10, 0x04, 0x04}) {
			t.Errorf("%s: wrote % x", tc.name, f.written)
		}
	}
	if _, err := newDriver(t, false).Status(context.Background(), &fake{}); !errors.Is(err, driver.ErrNoReadback) {
		t.Errorf("silent: %v", err)
	}
}

func job(st *driver.Status) *driver.Job {
	return &driver.Job{
		Pages:  []*image.Gray{img(16, 2, func(int, int) bool { return true })},
		Media:  &devicedb.Media{ID: "paper-80"},
		Status: st,
	}
}

var okStatus = &driver.Status{Code: driver.OK}

func TestPrintConfirmed(t *testing.T) {
	f := &fake{replies: [][]byte{{0x00}}}
	res, err := newDriver(t, false).Print(context.Background(), f, job(okStatus))
	if err != nil || !res.Confirmed || res.Message != "printed" {
		t.Fatalf("%+v %v", res, err)
	}
	if last := f.written[len(f.written)-1]; !bytes.Equal(last, []byte{0x1D, 0x72, 0x01}) {
		t.Errorf("last write % x", last)
	}
}

func TestPrintUnconfirmed(t *testing.T) {
	res, err := newDriver(t, false).Print(context.Background(), &fake{}, job(okStatus))
	if err != nil || res.Confirmed || res.Message != "sent (unconfirmed: no answer to GS r)" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPrintPaperEndDuringJob(t *testing.T) {
	_, err := newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x0C}}}, job(okStatus))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.NoMedia {
		t.Errorf("err = %v", err)
	}
}

func TestPrintRefusesBadStatus(t *testing.T) {
	// given status
	f := &fake{}
	_, err := newDriver(t, false).Print(context.Background(), f, job(&driver.Status{Code: driver.NoMedia, Detail: "out of paper"}))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.NoMedia || len(f.written) != 0 {
		t.Errorf("err = %v, wrote %d", err, len(f.written))
	}
}

func TestPrintWithoutStatusDoesNotWait(t *testing.T) {
	f := &fake{}
	d := newDriver(t, false)
	d.confirm = time.Hour // a wait would hang the test
	res, err := d.Print(context.Background(), f, job(nil))
	if err != nil || res.Confirmed || !strings.Contains(res.Message, "doesn't report status") {
		t.Fatalf("%+v %v", res, err)
	}
	for _, w := range f.written {
		if len(w) == 3 && (w[0] == 0x10 || w[0] == 0x1D && w[1] == 0x72) {
			t.Errorf("wrote a query: % x", w)
		}
	}
}

func TestStatusDropsStaleBytes(t *testing.T) {
	f := &fake{in: []byte{0x12, 0x12}, replies: [][]byte{{0x12}, {0x16}, {0x12}}}
	st, err := newDriver(t, false).Status(context.Background(), f)
	if err != nil || st.Code != driver.CoverOpen {
		t.Errorf("%+v %v", st, err)
	}
}

func TestPrintDropsStaleBeforeConfirm(t *testing.T) {
	f := &fake{in: []byte{0x00}} // a late answer from before must not confirm this job
	res, err := newDriver(t, false).Print(context.Background(), f, job(okStatus))
	if err != nil || res.Confirmed {
		t.Errorf("%+v %v", res, err)
	}
}

func TestStatusCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newDriver(t, false).Status(ctx, &fake{replies: [][]byte{{1}, {1}, {1}}}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestPrintCtxEndsAfterSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDriver(t, false)
	d.confirm = time.Minute
	res, err := d.Print(ctx, &fake{onGSR: cancel}, job(okStatus)) // the label is sent by then
	if err != nil || res.Confirmed || !strings.HasPrefix(res.Message, "sent (unconfirmed: stopped waiting: ") {
		t.Errorf("%+v %v", res, err)
	}
}

func TestPrintLinkDropped(t *testing.T) {
	_, err := newDriver(t, false).Print(context.Background(), &fake{readErr: io.EOF}, job(okStatus))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError || !errors.Is(err, io.EOF) || !strings.Contains(de.Detail, "may have printed") {
		t.Errorf("err = %v", err)
	}
}

func TestStatusHardError(t *testing.T) {
	if _, err := newDriver(t, false).Status(context.Background(), &fake{readErr: io.EOF}); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v", err)
	}
}

func TestEncodeClampsToHead(t *testing.T) {
	got := encode(t, newDriver(t, false), img(600, 1, func(int, int) bool { return true }))
	if want := []byte{0x1D, 0x76, 0x30, 0x00, 72, 0}; !bytes.Equal(got[2:8], want) { // 576 dots = 72 bytes
		t.Errorf("header % x", got[2:8])
	}
}

func TestStatusPartialAnswersNoReadback(t *testing.T) {
	for _, r := range [][][]byte{{{0x12}, nil, {0x12}}, {{0x12}, {0x12}, nil}, {{0x12}, {0x00}, {0x12}}} { // 0x00 isn't a DLE EOT reply
		if _, err := newDriver(t, false).Status(context.Background(), &fake{replies: r}); !errors.Is(err, driver.ErrNoReadback) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestPrintZeroReadsDoNotSpin(t *testing.T) {
	d := newDriver(t, false)
	d.confirm = 50 * time.Millisecond
	start := time.Now()
	res, err := d.Print(context.Background(), &fake{zero: true}, job(okStatus))
	if err != nil || res.Confirmed || time.Since(start) > time.Second {
		t.Errorf("%+v %v %v", res, err, time.Since(start))
	}
}

func TestPrintNearEndWarns(t *testing.T) {
	res, err := newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x03}}}, job(okStatus))
	if err != nil || !res.Confirmed || len(res.Warnings) != 1 || res.Warnings[0] != "paper near end" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestPrintNilMedia(t *testing.T) {
	j := job(okStatus)
	j.Media = nil
	if _, err := newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x00}}}, j); err != nil {
		t.Error(err)
	}
}

func TestPrintNearEndWarnsOnce(t *testing.T) {
	st := &driver.Status{Code: driver.OK, Detail: "paper near end"}
	res, err := newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x03}}}, job(st))
	if err != nil || len(res.Warnings) != 1 {
		t.Errorf("%+v %v", res, err)
	}
}

// Unsolicited ASB frames (the printer may have automatic status back on) are not the GS r answer: a
// cover-open frame neither confirms the job nor reads as paper end, and the real answer still counts.
func TestPrintASBFrameIsNotTheAnswer(t *testing.T) {
	res, err := newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x38, 0x0C, 0x0C, 0x00, 0x00}}}, job(okStatus))
	if err != nil || !res.Confirmed {
		t.Errorf("frame, then answer: %+v %v", res, err)
	}
	res, err = newDriver(t, false).Print(context.Background(), &fake{replies: [][]byte{{0x30, 0x00, 0x00, 0x00}}}, job(okStatus))
	if err != nil || res.Confirmed {
		t.Errorf("frame only: %+v %v", res, err)
	}
}

// Dropping stale bytes doesn't wait a whole read timeout: the driver shortens it while draining.
func TestDrainShortensReadTimeout(t *testing.T) {
	f := &fake{replies: [][]byte{{0x12}, {0x12}, {0x12}}}
	if _, err := newDriver(t, false).Status(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if len(f.timeout) != 2 || f.timeout[0] != drainWait || f.timeout[1] != 0 {
		t.Errorf("SetReadTimeout calls %v, want [%v 0]", f.timeout, drainWait)
	}
}

// An ASB frame split by drain (its first byte read there, the rest after it) is still skipped whole: its
// payload byte 0x00 is not the GS r answer.
func TestPrintSplitASBFrameIsNotTheAnswer(t *testing.T) {
	f := &fake{in: []byte{0x38}, replies: [][]byte{{0x00, 0x00, 0x00}}} // no GS r answer of its own
	res, err := newDriver(t, false).Print(context.Background(), f, job(okStatus))
	if err != nil || res.Confirmed {
		t.Errorf("%+v %v", res, err)
	}
}
