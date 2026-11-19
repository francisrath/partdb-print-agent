package printing

import (
	"bytes"
	"context"
	"errors"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
	"image"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/devicedb"
	"github.com/francisrath/partdb-print-agent/internal/driver"
	"github.com/francisrath/partdb-print-agent/internal/pdfgen"
	"github.com/francisrath/partdb-print-agent/internal/render"
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
	return &config.Printer{ID: "pt", Model: "PT-9500PC", Media: media, Cut: cut,
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
	if m, k := header(t, sent, 'M'), header(t, sent, 'K'); m != 0x40 || k != 0x00 {
		t.Errorf("ESC i M %02x K %02x, want 40 00 (auto cut, chain printing: no leader scrap)", m, k)
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
	if m, k := header(t, f.sent(), 'M'), header(t, f.sent(), 'K'); m != 0x00 || k != 0x04 {
		t.Errorf("ESC i M %02x K %02x, want 00 04 (half cut strip, chain printing)", m, k)
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
	if m, k := header(t, f.sent(), 'M'), header(t, f.sent(), 'K'); m != 0x40 || k != 0x00 {
		t.Errorf("ESC i M %02x K %02x: want chain (40 00) instead of half cut on non-laminated tape", m, k)
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
	s := service(t, f)
	_, err := s.Print(context.Background(), ptPrinter("", ""), "", labels(1))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError {
		t.Fatalf("err = %v, want COMMS_ERROR", err)
	}
	f.mu.Lock()
	f.failPages, f.queue = false, nil // plugged back in
	f.mu.Unlock()
	if res, err := s.Print(context.Background(), ptPrinter("", ""), "", labels(1)); err != nil || !res.Confirmed {
		t.Errorf("next job after replug: %+v %v", res, err)
	}
}

// goneWhileWaiting loses the device after a page was sent, while waiting for its status.
type goneWhileWaiting struct{ *fakePT }

func (g goneWhileWaiting) Read(p []byte) (int, error) {
	g.mu.Lock()
	sentPage := len(g.writes) > 1
	g.mu.Unlock()
	if sentPage {
		return 0, syscall.ENODEV
	}
	return g.fakePT.Read(p)
}

func TestPrintPTDeviceGoneWhileWaiting(t *testing.T) {
	f := newFakePT()
	s := service(t, f)
	s.OpenTransport = func(*config.Printer) driver.Transport { return goneWhileWaiting{f} }
	_, err := s.Print(context.Background(), ptPrinter("", ""), "", labels(1))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError {
		t.Fatalf("err = %v, want COMMS_ERROR", err)
	}
}

// A status check during a PT job answers "busy" at once and never sends ESC i S to a
// printing PT (the reference forbids it); it doesn't wait for the job either.
func TestStatusDuringPTJobIsBusy(t *testing.T) {
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
	start := time.Now()
	st, err := s.Status(context.Background(), ptPrinter("", ""))
	if err != nil || st.Code != driver.Busy || time.Since(start) > time.Second {
		t.Errorf("status during a job: %+v %v after %v, want BUSY at once", st, err, time.Since(start))
	}
	close(f.gate)
	if err := <-printed; err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range f.writes {
		if bytes.HasSuffix(w, []byte{0x1b, 'i', 'S'}) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d status requests sent; only the job's own may be", n)
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
	m, _ := devicedb.Default().Media("tze-36")
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
	m, _ := devicedb.Default().Media("tze-36")
	if _, err := s.Render(pdfgen.Page(36, 20, "0 g 0 0 36 20 re f"), ptPrinter("tze-36", ""), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "trimmed") {
		t.Error("no warning when the label has ink on the unprintable tape edges")
	}
}

func TestPTTestCardFitsPrintArea(t *testing.T) {
	s, log := logged(t)
	m, _ := devicedb.Default().Media("tze-36")
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

// silent is a printer that takes bytes but never answers.
type silent struct {
	kind string
	sent bytes.Buffer
}

func (s *silent) Open(context.Context) error  { return nil }
func (s *silent) Write(p []byte) (int, error) { return s.sent.Write(p) }
func (s *silent) Read([]byte) (int, error)    { return 0, os.ErrDeadlineExceeded }
func (s *silent) Close() error                { return nil }
func (s *silent) Kind() string                { return s.kind }

// A PT that doesn't answer can't print (it paces labels on its replies): COMMS_ERROR at
// once, before the PDF is fetched – not "set media in the config".
func TestPrintSilentPTFailsFast(t *testing.T) {
	s := service(t, nil)
	s.OpenTransport = func(*config.Printer) driver.Transport { return &silent{kind: "usb"} }
	fetched := false
	_, err := s.Print(context.Background(), ptPrinter("", ""), "", func(*driver.Media) ([]byte, error) {
		fetched = true
		return nil, errors.New("not reached")
	})
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.CommsError || fetched {
		t.Fatalf("err = %v, fetched = %v; want COMMS_ERROR before fetching", err, fetched)
	}
}

// A QL that gives no status (SNMP off, no ESC i S reply) prints blind, as it always has.
func TestPrintSilentQLPrintsBlind(t *testing.T) {
	s := service(t, nil)
	tr := &silent{kind: "tcp"}
	s.OpenTransport = func(*config.Printer) driver.Transport { return tr }
	p := &config.Printer{ID: "ql", Model: "QL-810W", Media: "dk-22210", NoSNMP: true,
		Transport: config.Transport{Kind: "tcp", Host: "127.0.0.1", Port: 9100}}
	res, err := s.Print(context.Background(), p, "", PDF(pdfgen.Page(29, 46, pdfgen.Text("QL"))))
	if err != nil {
		t.Fatal(err)
	}
	if res.Confirmed || res.Message != "sent (unconfirmed: printer gives no status)" || !bytes.HasSuffix(tr.sent.Bytes(), []byte{0x1a}) {
		t.Errorf("result %+v, sent %d bytes", res, tr.sent.Len())
	}
}

// Ink trimmed off the tape edges reaches the job result (and so Part-DB's job list).
func TestPrintCarriesTrimWarning(t *testing.T) {
	f := newFakePT()
	res, err := service(t, f).Print(context.Background(), ptPrinter("", ""), "", PDF(pdfgen.Page(36, 20, "0 g 0 0 36 20 re f")))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "trimmed") {
		t.Errorf("warnings %q", res.Warnings)
	}
}

// A broken pdftoppm must not fail the job: the service falls back to PDFium.
func TestPdftoppmFallsBackToPDFium(t *testing.T) {
	s, log := logged(t)
	s.Pdftoppm = &render.Pdftoppm{Bin: "/nonexistent/pdftoppm", Info: "/nonexistent/pdfinfo"}
	m, _ := devicedb.Default().Media("tze-36")
	pages, err := s.Render(pdfgen.Page(36, 20, "0 g 9 0 18 20 re f"), ptPrinter("tze-36", ""), &m)
	if err != nil || len(pages) != 1 {
		t.Fatalf("%d pages, %v", len(pages), err)
	}
	if !strings.Contains(log.String(), "rendering with PDFium") {
		t.Errorf("fallback not logged:\n%s", log)
	}
}

// With pdftoppm installed, the tape path renders the same geometry as with PDFium.
func TestPTRendersWithPdftoppm(t *testing.T) {
	pp, err := render.FindPdftoppm("on")
	if err != nil || pp == nil {
		t.Skip("pdftoppm not installed")
	}
	s, log := logged(t)
	s.Pdftoppm = pp
	m, _ := devicedb.Default().Media("tze-36")
	pages, err := s.Render(pdfgen.Page(36, 20, "0 g 9 0 18 20 re f"), ptPrinter("tze-36", ""), &m)
	if err != nil {
		t.Fatal(err)
	}
	if w := pages[0].Bounds().Dx(); w != 384 {
		t.Fatalf("width %d dots, want 384", w)
	}
	if n := blackCols(pages[0], 10); n < 254 || n > 258 {
		t.Errorf("18 mm bar is %d dots, want ~256", n)
	}
	// A label that doesn't fit is refused without trying PDFium too.
	if _, err := s.Render(pdfgen.Page(62, 20, "0 g 0 0 1 1 re f"), ptPrinter("tze-36", ""), &m); !errors.Is(err, render.ErrSize) {
		t.Errorf("size check: %v", err)
	}
	if s.r != nil || strings.Contains(log.String(), "PDFium") {
		t.Errorf("fell back to PDFium:\n%s", log)
	}
}

// quadrantInk reports which quadrants of img have dark pixels: [top-left, top-right, bottom-left, bottom-right].
func quadrantInk(img *image.Gray) [4]bool {
	var q [4]bool
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := range h {
		for x := range w {
			if img.Pix[y*img.Stride+x] < 128 {
				q[2*(2*y/h)+2*x/w] = true
			}
		}
	}
	return q
}

// A small square near the label's top-left corner shows where each fix moves the picture.
func TestRenderFixes(t *testing.T) {
	m, _ := devicedb.Default().Media("tze-36")
	pdf := pdfgen.Page(36, 20, "0 g 9 14 4 4 re f")
	for _, tc := range []struct {
		name               string
		mirror, upsideDown bool
		want               [4]bool
	}{
		{"none", false, false, [4]bool{true, false, false, false}},
		{"mirror", true, false, [4]bool{false, true, false, false}},
		{"upside down", false, true, [4]bool{false, false, false, true}},
		{"both", true, true, [4]bool{false, false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := logged(t)
			p := ptPrinter("tze-36", "")
			p.Mirror, p.UpsideDown = tc.mirror, tc.upsideDown
			pages, err := s.Render(pdf, p, &m)
			if err != nil {
				t.Fatal(err)
			}
			if got := quadrantInk(pages[0]); got != tc.want {
				t.Errorf("ink in quadrants %v, want %v", got, tc.want)
			}
		})
	}
}

// A Zebra prints the test card through the whole path: status (fresh connections), render at the
// configured label size, send, completion.
func TestPrintZebra(t *testing.T) {
	fake := &zpltest.Fake{Hold: time.Second} // a 4" label at 5 ips takes about 0.8 s
	host, port, _ := net.SplitHostPort(fake.Start(t))
	n, _ := strconv.Atoi(port)
	p := &config.Printer{ID: "zebra", Model: "GK420d", Media: "label-102x102", Transport: config.Transport{Kind: "tcp", Host: host, Port: n}}
	s := service(t, nil)
	res, err := s.Print(context.Background(), p, "", TestCard("GK420d", "test"))
	if err != nil || !res.Confirmed {
		t.Fatalf("%+v %v", res, err)
	}
	if f := fake.Formats(); len(f) != 1 || !strings.Contains(f[0], "^PW816^LL816") {
		t.Errorf("formats %.60q", f)
	}
}
