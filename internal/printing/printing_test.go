package printing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/driver/brotherpt"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
)

// fakePT plays a PT-9500PC with 36 mm laminated tape behind a driver.Transport: it answers
// ESC i S with the reply the real printer gave, and after each page (a write ending in
// 0x0C or 0x1A) reports a phase change and "printing completed".
type fakePT struct {
	mu        sync.Mutex
	writes    [][]byte
	queue     []byte
	width     byte
	failPages bool          // page writes fail with ENODEV (printer unplugged)
	gate      chan struct{} // page writes block until closed
	writing   chan struct{} // closed when the first page write starts
}

func newFakePT() *fakePT { return &fakePT{width: 36} }

func (f *fakePT) status(edit func(b []byte)) []byte {
	b := make([]byte, 32)
	copy(b, []byte{0x80, 0x20, 'B', '0', 'J', '0', 0, 0, 0, 0, f.width, 0x01})
	if edit != nil {
		edit(b)
	}
	return b
}

func (f *fakePT) Open(context.Context) error { return nil }
func (f *fakePT) Close() error               { return nil }
func (f *fakePT) Kind() string               { return "usb" }

func (f *fakePT) Write(p []byte) (int, error) {
	isStatus := bytes.HasSuffix(p, []byte{0x1b, 'i', 'S'})
	isPage := len(p) > 0 && (p[len(p)-1] == 0x0c || p[len(p)-1] == 0x1a)
	if isPage {
		f.mu.Lock()
		if f.writing != nil {
			close(f.writing)
			f.writing = nil
		}
		gate, fail := f.gate, f.failPages
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if fail {
			return 0, syscall.ENODEV
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	switch {
	case isStatus:
		f.queue = append(f.queue, f.status(nil)...)
	case isPage:
		f.queue = append(f.queue, f.status(func(b []byte) { b[18], b[19] = 0x06, 0x01 })...)
		f.queue = append(f.queue, f.status(func(b []byte) { b[18] = 0x01 })...)
	}
	return len(p), nil
}

func (f *fakePT) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.queue)
	f.queue = f.queue[n:]
	return n, nil
}

func (f *fakePT) sent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Join(f.writes, nil)
}

func ptPrinter(media, cut string) *config.Printer {
	return &config.Printer{ID: "pt", Driver: "brother-pt", Model: "PT-9500PC", Media: media, Cut: cut,
		Transport: config.Transport{Kind: "usb", Device: "/dev/usb/lp0"}}
}

func service(t *testing.T, f *fakePT) *Service {
	t.Helper()
	s := &Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if f != nil {
		s.OpenTransport = func(*config.Printer) driver.Transport { return f }
	}
	t.Cleanup(s.Close)
	return s
}

// labels returns an n-page PDF of 36×50 mm labels (long side along the tape).
func labels(n int) Source {
	pages := make([]string, n)
	for i := range pages {
		pages[i] = pdfgen.Text("PT")
	}
	return PDF(pdfgen.Pages(36, 50, pages...))
}

func header(t *testing.T, sent []byte, cmd byte) byte {
	t.Helper()
	i := bytes.Index(sent, []byte{0x1b, 'i', cmd})
	if i < 0 {
		t.Fatalf("no ESC i %c in job", cmd)
	}
	return sent[i+3]
}

func TestPrintPTSinglePage(t *testing.T) {
	f := newFakePT()
	res, err := service(t, f).Print(context.Background(), ptPrinter("", ""), "", labels(1))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || res.Media != "tze-36" || res.Pages != 1 {
		t.Errorf("result %+v", res)
	}
	sent := f.sent()
	if sent[len(sent)-1] != 0x1a {
		t.Errorf("job ends %02x", sent[len(sent)-1])
	}
	if m := header(t, sent, 'M'); m != 0x40 {
		t.Errorf("ESC i M %02x, want 40 (cut each label)", m)
	}
}

func TestPrintPTBatchHalfCut(t *testing.T) {
	f := newFakePT()
	res, err := service(t, f).Print(context.Background(), ptPrinter("", ""), "", labels(3))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmed || res.Pages != 3 {
		t.Errorf("result %+v", res)
	}
	if k := header(t, f.sent(), 'K'); k&0x04 == 0 {
		t.Errorf("ESC i K %02x: no half cut in a batch on laminated tape", k)
	}
	if len(f.queue) != 0 {
		t.Errorf("%d status bytes left unread: not every page was confirmed", len(f.queue))
	}
}

func TestPrintPTBatchFullCutOnNonLaminated(t *testing.T) {
	f := newFakePT()
	s := service(t, f)
	s.OpenTransport = func(*config.Printer) driver.Transport { return &nonLaminated{f} }
	if _, err := s.Print(context.Background(), ptPrinter("", "half"), "", labels(2)); err != nil {
		t.Fatal(err)
	}
	if k := header(t, f.sent(), 'K'); k&0x04 != 0 {
		t.Errorf("ESC i K %02x: half cut on non-laminated tape", k)
	}
}

// nonLaminated reports media type 03 in every status.
type nonLaminated struct{ *fakePT }

func (n *nonLaminated) Read(p []byte) (int, error) {
	k, err := n.fakePT.Read(p)
	for i := 0; i+32 <= k; i += 32 {
		p[i+11] = 0x03
	}
	return k, err
}

func TestPrintPTMediaMismatch(t *testing.T) {
	f := newFakePT()
	_, err := service(t, f).Print(context.Background(), ptPrinter("tze-24", ""), "", labels(1))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.MediaMismatch {
		t.Fatalf("err = %v, want MEDIA_MISMATCH", err)
	}
	if len(f.writes) != 1 {
		t.Errorf("%d writes; only the status request may be sent", len(f.writes))
	}
}

func TestPrintPTDeviceGone(t *testing.T) {
	f := newFakePT()
	f.failPages = true
	_, err := service(t, f).Print(context.Background(), ptPrinter("", ""), "", labels(1))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError {
		t.Fatalf("err = %v, want COMMS_ERROR", err)
	}
}

func TestStatusWaitsForJob(t *testing.T) {
	f := newFakePT()
	f.gate, f.writing = make(chan struct{}), make(chan struct{})
	writing := f.writing
	s := service(t, f)
	printed := make(chan error, 1)
	go func() {
		_, err := s.Print(context.Background(), ptPrinter("", ""), "", labels(1))
		printed <- err
	}()
	<-writing // the job is sending its page
	statusDone := make(chan struct{})
	go func() {
		_, _ = s.Status(context.Background(), ptPrinter("", ""))
		close(statusDone)
	}()
	select {
	case <-statusDone:
		t.Fatal("status request sent while a job was printing")
	case <-time.After(200 * time.Millisecond):
	}
	close(f.gate)
	if err := <-printed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-statusDone:
	case <-time.After(5 * time.Second):
		t.Fatal("status never ran after the job")
	}
}

func TestPrintPTDryRun(t *testing.T) {
	out := filepath.Join(t.TempDir(), "job.bin")
	s := service(t, nil)
	s.OpenTransport = func(*config.Printer) driver.Transport { t.Fatal("transport opened on a dry run"); return nil }
	res, err := s.Print(context.Background(), ptPrinter("tze-36", ""), out, labels(2))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil || len(got) != res.Bytes {
		t.Fatalf("%d bytes, result says %d: %v", len(got), res.Bytes, err)
	}
	if !bytes.HasPrefix(got, make([]byte, 100)) || got[len(got)-1] != 0x1a {
		t.Error("dry-run file isn't a whole PT job")
	}
}

// logged returns a Service whose log goes to the returned buffer.
func logged(t *testing.T) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s := &Service{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	t.Cleanup(s.Close)
	return s, &buf
}

func blackCols(img *image.Gray, y int) int {
	n := 0
	for x := range img.Bounds().Dx() {
		if img.Pix[y*img.Stride+x] < 128 {
			n++
		}
	}
	return n
}

// Tape labels print at true size: a 36 mm-wide label is drawn across the full tape
// (512 dots) and the unprintable 4.5 mm edges are trimmed, so 18 mm stay 18 mm.
func TestPTRendersAtTapeWidth(t *testing.T) {
	s, log := logged(t)
	m, _ := brotherpt.LookupMedia("tze-36")
	pages, err := s.Render(pdfgen.Page(36, 20, "0 g 9 0 18 20 re f"), ptPrinter("tze-36", ""), &m)
	if err != nil {
		t.Fatal(err)
	}
	if w := pages[0].Bounds().Dx(); w != 384 {
		t.Fatalf("width %d dots, want 384", w)
	}
	if n := blackCols(pages[0], 10); n < 254 || n > 258 {
		t.Errorf("18 mm bar is %d dots, want ~256 (1:1 at 360 dpi)", n)
	}
	if strings.Contains(log.String(), "trimmed") {
		t.Errorf("warned about trimmed ink on a label with clear edges:\n%s", log)
	}
}

func TestPTWarnsWhenEdgeInked(t *testing.T) {
	s, log := logged(t)
	m, _ := brotherpt.LookupMedia("tze-36")
	if _, err := s.Render(pdfgen.Page(36, 20, "0 g 0 0 36 20 re f"), ptPrinter("tze-36", ""), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "trimmed") {
		t.Error("no warning when the label has ink on the unprintable tape edges")
	}
}

func TestPTTestCardFitsPrintArea(t *testing.T) {
	s, log := logged(t)
	m, _ := brotherpt.LookupMedia("tze-36")
	pdf, err := TestCard("PT-9500PC", "test")(&m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Render(pdf, ptPrinter("tze-36", ""), &m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "trimmed") {
		t.Errorf("test card loses its border on the tape edges:\n%s", log)
	}
}
