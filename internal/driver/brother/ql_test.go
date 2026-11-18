package brother

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/snmp"
)

var update = flag.Bool("update", false, "rewrite golden files")

func unpackbits(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); {
		n := int(int8(src[i]))
		i++
		if n >= 0 {
			out = append(out, src[i:i+n+1]...)
			i += n + 1
		} else {
			out = append(out, bytes.Repeat(src[i:i+1], 1-n)...)
			i++
		}
	}
	return out
}

func TestPackbitsRoundTrip(t *testing.T) {
	long := bytes.Repeat([]byte{1, 2, 3}, 100)
	for _, in := range [][]byte{
		{}, {7}, {7, 7}, {1, 2}, make([]byte, 90), make([]byte, 300), long,
		{0, 0, 1, 2, 2, 2, 3, 0, 0xff, 0xff},
		append(bytes.Repeat([]byte{9}, 129), 1, 2, 3),
	} {
		got := unpackbits(packbits(in))
		if !bytes.Equal(got, in) {
			t.Errorf("roundtrip %v: got %v", in, got)
		}
	}
	if got := packbits(make([]byte, 90)); !bytes.Equal(got, []byte{257 - 90, 0}) {
		t.Errorf("blank line = % x", got)
	}
}

func TestPackLineMapping(t *testing.T) {
	spec := dk(t, "dk-22210")
	img := white(306, 1)
	img.Pix[0] = 0   // leftmost dot
	img.Pix[305] = 0 // rightmost dot
	line := packLine(img, 0, spec)
	// rightmost image column → pin 6, leftmost → pin 6+305 = 311
	if line[0] != 0x80>>6 || line[311/8] != 0x80>>(311%8) {
		t.Fatalf("line = % x", line)
	}
}

func TestParseStatus(t *testing.T) {
	base := func() []byte {
		b := make([]byte, 32)
		b[0], b[1], b[2], b[3], b[4] = 0x80, 0x20, 'B', '4', 0x39
		b[10], b[11] = 29, 0x0a
		return b
	}
	tests := []struct {
		name  string
		mod   func(b []byte)
		code  driver.Code
		media string
		done  bool
	}{
		{"ok 29mm", func(b []byte) {}, driver.OK, "dk-22210", false},
		{"die-cut 17x54", func(b []byte) { b[10], b[11], b[17] = 17, 0x0b, 54 }, driver.OK, "dk-11204", false},
		{"no media", func(b []byte) { b[8], b[10], b[11] = 0x01, 0, 0 }, driver.NoMedia, "", false},
		{"cover open", func(b []byte) { b[9], b[18] = 0x10, 0x02 }, driver.CoverOpen, "dk-22210", false},
		{"wrong media", func(b []byte) { b[9], b[18] = 0x01, 0x02 }, driver.MediaMismatch, "dk-22210", false},
		{"printed", func(b []byte) { b[18] = 0x01 }, driver.OK, "dk-22210", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.mod(b)
			s, err := parseQLStatus(b)
			if err != nil {
				t.Fatal(err)
			}
			if s.Code != tc.code || s.Done != tc.done || s.Model != "QL-810W" {
				t.Errorf("got %+v", s)
			}
			if (s.Media == nil && tc.media != "") || (s.Media != nil && s.Media.ID != tc.media) {
				t.Errorf("media = %+v, want %q", s.Media, tc.media)
			}
		})
	}
	if _, err := parseQLStatus([]byte("garbage")); err == nil {
		t.Error("garbage parsed")
	}
}

func white(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	return img
}

// testPage draws a border and a diagonal, enough to catch orientation/offset changes.
func testPage(w, h int) *image.Gray {
	img := white(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < 4 || y < 4 || x >= w-4 || y >= h-4 || x == y*w/h {
				img.Pix[y*img.Stride+x] = 0
			}
		}
	}
	return img
}

func TestEncodeGolden(t *testing.T) {
	tests := []struct {
		name  string
		media string
		pages []*image.Gray
		job   driver.Job
		raw   bool
	}{
		{"29mm_one_page", "dk-22210", []*image.Gray{testPage(306, 100)}, driver.Job{}, false},
		{"29mm_two_pages_cut_last", "dk-22210", []*image.Gray{testPage(306, 50), testPage(306, 60)}, driver.Job{Cut: "last"}, false},
		{"12mm_uncompressed", "dk-22214", []*image.Gray{testPage(106, 40)}, driver.Job{}, true},
		{"17x54_diecut", "dk-11204", []*image.Gray{testPage(165, 566)}, driver.Job{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := dk(t, tc.media)
			d := newQLForTest(t)
			d.noCompression = tc.raw
			job := tc.job
			job.Pages, job.Media = tc.pages, &m
			var buf bytes.Buffer
			if err := d.Encode(&buf, &job); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "ql_"+tc.name+".bin")
			if *update {
				if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("output differs from %s (run with -update after verifying on hardware)", golden)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	d := newQLForTest(t)
	check := func(pages []*image.Gray, cut, media string) error {
		m := dk(t, media)
		return d.validate(&driver.Job{Pages: pages, Cut: cut, Media: &m})
	}
	var de *driver.Error
	if err := check([]*image.Gray{white(696, 10)}, "", "dk-22210"); !errors.As(err, &de) || de.Code != driver.MediaMismatch {
		t.Errorf("62mm page on 29mm tape: %v", err)
	}
	if err := check([]*image.Gray{white(165, 600)}, "", "dk-11204"); err == nil {
		t.Error("too-long die-cut label accepted")
	}
	if err := check([]*image.Gray{white(306, 11811+1)}, "", "dk-22210"); err == nil {
		t.Error("continuous label over 1 m accepted")
	}
	if err := check([]*image.Gray{white(306, 10)}, "sometimes", "dk-22210"); err == nil {
		t.Error("bad cut mode accepted")
	}
}

// eofTransport is a printer connection that was closed.
type eofTransport struct{}

func (eofTransport) Open(context.Context) error  { return nil }
func (eofTransport) Write(p []byte) (int, error) { return len(p), nil }
func (eofTransport) Read([]byte) (int, error)    { return 0, io.EOF }
func (eofTransport) Close() error                { return nil }
func (eofTransport) Kind() string                { return "test" }

func TestWaitDoneStopsOnClosedConnection(t *testing.T) {
	start := time.Now()
	err := waitDone(context.Background(), eofTransport{}, 1, 30*time.Second)
	if !errors.Is(err, driver.ErrNoReadback) || !errors.Is(err, io.EOF) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("spun for %v on a closed connection", time.Since(start))
	}
}

// busyTransport keeps answering with a valid "phase change" status that never completes.
type busyTransport struct{ eofTransport }

func (busyTransport) Read(p []byte) (int, error) {
	b := make([]byte, 32)
	b[0], b[1], b[2], b[4], b[18] = 0x80, 0x20, 'B', 0x39, 0x06
	return copy(p, b), nil
}

func TestWaitDoneHonoursDeadline(t *testing.T) {
	start := time.Now()
	err := waitDone(context.Background(), busyTransport{}, 1, 200*time.Millisecond)
	if !errors.Is(err, driver.ErrNoReadback) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
}

func dk(t *testing.T, id string) driver.Media {
	t.Helper()
	m, ok := devicedb.Default().Media(id)
	if !ok {
		t.Fatalf("no media %s", id)
	}
	return m
}

func newQLForTest(t *testing.T) *ql {
	t.Helper()
	m, _ := devicedb.Default().Model("QL-810W")
	d, err := driver.New(m, driver.Options{NoSNMP: true})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*ql)
}

// silentTCP is a network QL that takes the job but never answers on 9100.
type silentTCP struct{ sent bytes.Buffer }

func (s *silentTCP) Open(context.Context) error  { return nil }
func (s *silentTCP) Write(p []byte) (int, error) { return s.sent.Write(p) }
func (s *silentTCP) Read([]byte) (int, error)    { return 0, os.ErrDeadlineExceeded }
func (s *silentTCP) Close() error                { return nil }
func (s *silentTCP) Kind() string                { return "tcp" }

func qlJob(t *testing.T, st *driver.Status) *driver.Job {
	m := dk(t, "dk-22210")
	return &driver.Job{Pages: []*image.Gray{testPage(306, 100)}, Media: &m, Status: st}
}

// With SNMP blocked and no TCP status, the QL prints blind and says so, as it always has.
func TestQLPrintWithoutSNMP(t *testing.T) {
	d := newQLForTest(t)
	d.query = func(context.Context) (*snmp.State, error) { return nil, errors.New("udp 161 blocked") }
	tr := &silentTCP{}
	if _, err := d.Status(context.Background(), tr); !errors.Is(err, driver.ErrNoReadback) {
		t.Fatalf("status: %v, want ErrNoReadback", err)
	}
	tr.sent.Reset() // only the job counts from here
	job := qlJob(t, nil)
	res, err := d.Print(context.Background(), tr, job)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := d.Encode(&want, job); err != nil {
		t.Fatal(err)
	}
	if res.Confirmed || res.Message != "sent (unconfirmed: printer gives no status)" || !bytes.Equal(tr.sent.Bytes(), want.Bytes()) {
		t.Errorf("result %+v, sent %d bytes, want the %d-byte job", res, tr.sent.Len(), want.Len())
	}
}

// Over SNMP, a job sent to an idle printer is confirmed when it goes printing → idle.
func TestQLPrintConfirmsViaSNMP(t *testing.T) {
	d := newQLForTest(t)
	seq := []snmp.State{{Printing: true}, {Idle: true}, {Idle: true}, {Idle: true}, {Idle: true}, {Idle: true}}
	d.query = func(context.Context) (*snmp.State, error) {
		s := seq[0]
		if len(seq) > 1 {
			seq = seq[1:]
		}
		return &s, nil
	}
	d.poll = time.Millisecond
	st := &driver.Status{Code: driver.OK, Via: "snmp", Idle: true}
	res, err := d.Print(context.Background(), &silentTCP{}, qlJob(t, st))
	if err != nil || !res.Confirmed || res.Message != "printed" {
		t.Fatalf("%+v %v", res, err)
	}
	// busy before sending: printing seen may be another job's
	seq = []snmp.State{{Printing: true}, {Idle: true}, {Idle: true}, {Idle: true}, {Idle: true}, {Idle: true}}
	st.Idle = false
	res, err = d.Print(context.Background(), &silentTCP{}, qlJob(t, st))
	if err != nil || res.Confirmed {
		t.Fatalf("busy before sending: %+v %v", res, err)
	}
}

// Brother's raster reference (p.33): "The QL-800 does not support the compression mode."
func TestQL800SendsUncompressed(t *testing.T) {
	m, _ := devicedb.Default().Model("QL-800")
	d, err := driver.New(m, driver.Options{NoSNMP: true})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := d.Encode(&b, qlJob(t, nil)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.Bytes(), []byte{'M', 0, 'g', 0, 90}) || bytes.Contains(b.Bytes(), []byte{'M', 2}) {
		t.Error("QL-800 job isn't uncompressed (M 0, 90-byte raster lines)")
	}
}

// The 12.7 mm minimum length is for continuous tape; a die-cut label is as long as it is.
func TestQLShortDieCutNotPadded(t *testing.T) {
	m := dk(t, "dk-11204")
	var b bytes.Buffer
	if err := newQLForTest(t).Encode(&b, &driver.Job{Pages: []*image.Gray{testPage(165, 100)}, Media: &m}); err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(b.Bytes(), []byte{'g', 0}); n != 100 {
		t.Errorf("%d raster lines, want 100", n)
	}
}

func TestQLPrintCancelledSendsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &silentTCP{}
	if _, err := newQLForTest(t).Print(ctx, tr, qlJob(t, nil)); !errors.Is(err, context.Canceled) || tr.sent.Len() != 0 {
		t.Errorf("err %v, %d bytes sent", err, tr.sent.Len())
	}
}
