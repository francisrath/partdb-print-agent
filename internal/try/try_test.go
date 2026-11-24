package try_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/pdb-print/internal/config"
	"github.com/francisrath/pdb-print/internal/devicedb"
	"github.com/francisrath/pdb-print/internal/discovery"
	"github.com/francisrath/pdb-print/internal/driver"
	"github.com/francisrath/pdb-print/internal/driver/zpl/zpltest"
	"github.com/francisrath/pdb-print/internal/printing"
	"github.com/francisrath/pdb-print/internal/try"
)

// dialog plays the person at the printer: it waits for a question in the output, then answers.
type dialog struct {
	t    *testing.T
	mu   sync.Mutex
	out  strings.Builder
	seen int
	in   *io.PipeWriter
	done chan struct{}
	path string
	err  error
}

func (d *dialog) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.Write(p)
}

func (d *dialog) output() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.String()
}

// answer waits for prompt (after the last one answered) and types ans; hook runs just before.
func (d *dialog) answer(prompt, ans string, hook ...func()) {
	d.t.Helper()
	// generous: only a hung session waits it out, and the first render under -race on a busy CI
	// runner (PDFium compiled from scratch) has taken over 20 s
	for end := time.Now().Add(2 * time.Minute); ; time.Sleep(5 * time.Millisecond) {
		out := d.output()
		if i := strings.Index(out[d.seen:], prompt); i >= 0 {
			d.seen += i + len(prompt)
			break
		}
		select {
		case <-d.done:
			d.t.Fatalf("finished before asking %q; output:\n%s", prompt, out)
		default:
		}
		if time.Now().After(end) {
			d.t.Fatalf("not asked %q; output:\n%s", prompt, out)
		}
	}
	for _, h := range hook {
		h()
	}
	_, _ = io.WriteString(d.in, ans+"\n")
}

// finish closes the input (the person closes the window) and waits for the report.
func (d *dialog) finish() string {
	d.t.Helper()
	d.in.Close()
	select {
	case <-d.done:
	case <-time.After(2 * time.Minute):
		d.t.Fatalf("still running; output:\n%s", d.output())
	}
	if d.err != nil {
		d.t.Fatal(d.err)
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		d.t.Fatal(err)
	}
	return string(b)
}

// A measured length just over the label (804 dots: 100 mm + 0.5 mm gap) still suggests the size
// the printer accepts, not a shorter label further off.
func TestTrySuggestsAcceptedLength(t *testing.T) {
	fake := &zpltest.Fake{LabelDots: 804}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for 2 (100×100 mm", "")
	d.answer("Print the test labels now?", "n")
	d.finish()
}

// A label too short for a QR code doesn't ask about one.
func TestTryNoQRQuestionsOnShortLabels(t *testing.T) {
	fake := &zpltest.Fake{Hold: 300 * time.Millisecond, LabelDots: 116} // 13 mm + gap
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "16") // 25×13 mm
	d.answer("Print the test labels now?", "y")
	for _, q := range []string{"Did the labels come out", "whole frame", "TOP at the top edge", "mirrored", "dark enough", "checkerboard", "Is the barcode sharp", "barcode scanner"} {
		d.answer(q, "")
	}
	d.answer("each on its own label", "")
	d.finish()
	if out := d.output(); strings.Contains(out, "phone") || strings.Contains(out, "QR code and the barcode") {
		t.Errorf("asked about a QR code that wasn't printed:\n%s", out[strings.Index(out, "Label 2/5"):])
	}
}

func start(t *testing.T, addr string) *dialog {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	r, w := io.Pipe()
	d := &dialog{t: t, in: w, done: make(chan struct{})}
	svc := &printing.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(svc.Close)
	o := try.Options{In: r, Out: d, Port: p, Dir: t.TempDir(), Svc: svc, Version: "test", ReplyIdle: 100 * time.Millisecond, // the fake answers at once
		Discover: func(context.Context) ([]discovery.Candidate, error) {
			return []discovery.Candidate{{Kind: "tcp", Host: host, Port: p, Status: discovery.Unknown}}, nil
		}}
	go func() {
		defer close(d.done)
		d.path, d.err = try.Run(context.Background(), o)
	}()
	return d
}

func TestTryHappyPath(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "", func() {
		// all labels are sent before the first question: the tester may not be at the printer
		if n := waitFormats(fake, 4); n != 4 {
			t.Errorf("%d labels sent before the questions, want 4", n)
		}
	})
	for _, q := range []string{"whole frame", "TOP at the top edge", "mirrored", "dark enough", "checkerboard",
		"sharp", "phone", "barcode scanner", "each on its own label"} {
		d.answer(q, "")
	}
	d.answer("Open the printer's lid", "", func() { fake.Set(func(f *zpltest.Fake) { f.HeadUp, f.Errors, f.SilentHS = true, 0x4, true }) })
	d.answer("Close the lid again", "", func() { fake.Set(func(f *zpltest.Fake) { f.HeadUp, f.Errors, f.SilentHS = false, 0, false }) })
	got := d.finish()

	for _, want := range []string{"stays until the printer is switched off and on", "Zebra GK420d", "Labels: 100×100 mm", "Label 1/5: printed", "Label 2/5: printed", "Label 3a+3b/5: printed",
		"Label 4/5: skipped (square labels)", "Status head open: COVER_OPEN", "Status head closed: OK", "restore settings"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"28J000000001", "ZBR0000001", "127.0.0.1", "192.0.2.10", "192.0.2.11", "00:07:4D:00:00:01"} {
		if strings.Contains(got, secret) {
			t.Errorf("report contains %q: %s", secret, got[max(0, strings.Index(got, secret)-200):strings.Index(got, secret)+40])
		}
	}
	if f := fake.Formats(); len(f) != 4 {
		t.Errorf("%d labels printed, want 4 (1, 2, 3a, 3b)", len(f))
	}
	if !restored(fake) {
		t.Errorf("the printer's settings weren't sent back")
	}
	// the GK420d doesn't report mirror or orientation: don't claim those were restored
	if out := d.output(); strings.Contains(out, "back as they were") || !strings.Contains(out, "off and on") {
		t.Errorf("restore message:\n%s", out[max(0, len(out)-600):])
	}
}

func TestTryNothingBeforeConsent(t *testing.T) {
	fake := &zpltest.Fake{}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "n")
	if got := d.finish(); !strings.Contains(got, "Stopped before asking the printer anything") {
		t.Errorf("report:\n%s", got)
	}
	if r := fake.Received(); r != "" {
		t.Errorf("printer received %q", r)
	}
}

func TestTryNonZebraStops(t *testing.T) {
	fake := &zpltest.Fake{SilentHI: true}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	got := d.finish()
	if r := fake.Received(); r != "~HI\r\n" {
		t.Errorf("printer received %q, want only ~HI", r)
	}
	if !strings.Contains(d.output(), "EPL") || !strings.Contains(got, "Identify:") {
		t.Errorf("no explanation; output:\n%s\nreport:\n%s", d.output(), got)
	}
}

func TestTryUpsideDownRetry(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "")
	d.answer("whole frame", "")
	d.answer("TOP at the top edge", "n")
	d.answer("upside down", "y")
	for _, q := range []string{"mirrored", "dark enough", "checkerboard", "sharp", "phone", "barcode scanner", "each on its own label"} {
		d.answer(q, "")
	}
	d.answer("whole frame of label 1b/5", "y")
	d.answer("TOP at the top edge", "y")
	got := d.finish()
	f := fake.Formats()
	if len(f) != 5 || f[0] == f[4] || !strings.Contains(got, "Label 1b/5: printed") || !strings.Contains(got, "upside_down=true") {
		t.Errorf("%d formats; report:\n%s", len(f), got)
	}
}

func TestTryShiftRetry(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "")
	d.answer("whole frame", "n")
	d.answer("Which side", "l")
	d.answer("how many mm", "3")
	for _, q := range []string{"TOP at the top edge", "mirrored", "dark enough", "checkerboard", "sharp", "phone", "barcode scanner", "each on its own label"} {
		d.answer(q, "")
	}
	d.answer("whole frame of label 1b/5", "y")
	d.finish()
	if f := fake.Formats(); len(f) != 5 || !strings.Contains(f[4], "^LH24,0") {
		t.Errorf("retry format %.80q", f)
	}
}

// Closing the window halfway still writes the report and puts the printer's settings back.
func TestTryEOFWritesReport(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "y")
	got := d.finish()
	if !strings.Contains(got, "Label 1/5: printed") || !restored(fake) {
		t.Errorf("report:\n%s\nrestored %v", got, restored(fake))
	}
}

// Enter at "Which side" skips the fix but not the questions after it.
func TestTrySkipSideContinues(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "")
	d.answer("whole frame", "n")
	d.answer("Which side", "")
	d.answer("TOP at the top edge", "")
	d.finish()
}

// When the input ends (window closed) no further label is sent, even mid-fix.
func TestTryNoPrintAfterInputEnds(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "")
	d.answer("whole frame", "n")
	d.answer("Which side", "b")
	d.finish() // closed at the mm question
	if n := waitFormats(fake, 5); n != 4 {
		t.Errorf("%d labels sent, want only the first batch (4)", n)
	}
}

// waitFormats waits up to 2 s for at least n labels (the fake records them in its own goroutine)
// and returns how many arrived.
func waitFormats(fake *zpltest.Fake, n int) int {
	for end := time.Now().Add(2 * time.Second); len(fake.Formats()) < n && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	return len(fake.Formats())
}

// restored reports whether the printer got its own settings back: a format without a graphic.
// (Not "the last thing received": the fake reads connections concurrently, so on a busy machine
// a label's last bytes can be read after the restore was sent.)
// It waits up to 2 s: the fake records what it reads in its own goroutine.
func restored(fake *zpltest.Fake) bool {
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		for _, f := range fake.AllFormats() {
			if strings.Contains(f, "^PW799^LL812^LH0,0^LS0^LT0^XZ") && !strings.Contains(f, "^GF") {
				return true
			}
		}
	}
	return false
}

// fakeCat is a BLE cat printer like an MX10: after a job, busy to the next status request and then
// AE 00 ("buffer empty"); idle otherwise.
type fakeCat struct {
	mu       sync.Mutex
	in       []byte
	written  bytes.Buffer
	printing bool
}

func catFrame(cmd byte, payload ...byte) []byte {
	var crc byte
	for _, x := range payload {
		crc ^= x
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	f := append([]byte{0x51, 0x78, cmd, 0x00, byte(len(payload)), 0x00}, payload...)
	return append(f, crc, 0xFF)
}

func (f *fakeCat) Open(context.Context) error { return nil }
func (f *fakeCat) Close() error               { return nil }
func (f *fakeCat) Kind() string               { return "ble" }

func (f *fakeCat) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written.Write(p)
	switch {
	case !bytes.Equal(p, catFrame(0xA3, 0x00)):
		f.printing = true
	case f.printing:
		f.printing = false
		f.in = append(f.in, catFrame(0xA3, 0x80, 0x01, 0x95)...)
		f.in = append(f.in, catFrame(0xAE, 0x00)...)
	default:
		f.in = append(f.in, catFrame(0xA3, 0x00, 0x01, 0x95)...)
	}
	return len(p), nil
}

func (f *fakeCat) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		time.Sleep(time.Millisecond)
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, f.in)
	f.in = f.in[n:]
	return n, nil
}

func (f *fakeCat) sent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.written.Bytes())
}

// A cat printer: no Zebra identify, setup reads or restore; "too pale" reprints darker.
func TestTryCatPrinter(t *testing.T) {
	fake := &fakeCat{}
	m, _ := devicedb.Default().Model("MX10")
	r, w := io.Pipe()
	d := &dialog{t: t, in: w, done: make(chan struct{})}
	svc := &printing.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenTransport: func(*config.Printer) driver.Transport { return fake }}
	t.Cleanup(svc.Close)
	o := try.Options{In: r, Out: d, Dir: t.TempDir(), Svc: svc, Version: "test",
		Discover: func(context.Context) ([]discovery.Candidate, error) {
			return []discovery.Candidate{{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", BLEName: "MX10", Model: m, ModelID: "MX10", Status: discovery.Supported}}, nil
		}}
	go func() {
		defer close(d.done)
		d.path, d.err = try.Run(context.Background(), o)
	}()
	d.answer("Print the test labels now?", "y")
	d.answer("Did the labels come out", "")
	d.answer("whole frame", "")
	d.answer("sharp", "")
	d.answer("pale or patchy?", "y")
	got := d.finish()
	// darkness hardly changes an MX10's print: no darker reprint, the paper is the likely cause
	if sent := fake.sent(); bytes.Count(sent, catFrame(0xBD, 0xC0)) != 0 || bytes.Count(sent, catFrame(0xBD, 0x80)) != 2 {
		t.Errorf("speeds: %d dark, %d normal jobs; want the two labels only", bytes.Count(sent, catFrame(0xBD, 0xC0)), bytes.Count(sent, catFrame(0xBD, 0x80)))
	}
	// the raw A3 reply goes into the report, for telling clones apart
	for _, want := range []string{"MX10", "Label 1/2: printed", "Label 2/2: printed", "pale: check the paper", fmt.Sprintf("% x", catFrame(0xA3, 0x00, 0x01, 0x95))} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if out := d.output(); !strings.Contains(out, "thermal side") || !strings.Contains(out, "Darkness changes little") {
		t.Errorf("no paper advice:\n%s", out)
	}
	out := d.output()
	for _, zebra := range []string{"~HI", "Ask this printer what it is", "Read this printer's settings", "Which labels are loaded", "Switch the printer off", "restored", "Open the printer's lid"} {
		if strings.Contains(out, zebra) || strings.Contains(got, zebra) {
			t.Errorf("Zebra step %q in a cat printer test:\n%s", zebra, out)
		}
	}
}

// An unknown Bluetooth device in the list isn't taken for the Zebra.
func TestTryListsUnknownBLENeutrally(t *testing.T) {
	m, _ := devicedb.Default().Model("MX10")
	r, w := io.Pipe()
	d := &dialog{t: t, in: w, done: make(chan struct{})}
	svc := &printing.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(svc.Close)
	o := try.Options{In: r, Out: d, Dir: t.TempDir(), Svc: svc, Version: "test",
		Discover: func(context.Context) ([]discovery.Candidate, error) {
			return []discovery.Candidate{
				{Kind: "ble", Address: "AA:BB:CC:DD:EE:03", Status: discovery.Unknown},
				{Kind: "ble", Address: "AA:BB:CC:DD:EE:01", BLEName: "MX10", Model: m, ModelID: "MX10", Status: discovery.Supported},
			}, nil
		}}
	go func() {
		defer close(d.done)
		d.path, d.err = try.Run(context.Background(), o)
	}()
	d.answer("Which one is the printer to test?", "1")
	d.finish()
	if out := d.output(); strings.Contains(out, "Zebra)") || !strings.Contains(out, "Bluetooth device, not a known label printer") {
		t.Errorf("output:\n%s", out)
	}
}

// A Bluetooth Classic printer is found and named, never asked ~HI; try doesn't test ESC/POS yet.
func TestTryBTPrinter(t *testing.T) {
	m, _ := devicedb.Default().Model("RG-MLP80B")
	r, w := io.Pipe()
	d := &dialog{t: t, in: w, done: make(chan struct{})}
	svc := &printing.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(svc.Close)
	o := try.Options{In: r, Out: d, Dir: t.TempDir(), Svc: svc, Version: "test",
		Discover: func(context.Context) ([]discovery.Candidate, error) {
			return []discovery.Candidate{
				{Kind: "bt", Address: "AA:BB:CC:DD:EE:03", BTName: "Receipt", Status: discovery.Unknown},
				{Kind: "bt", Address: "AA:BB:CC:DD:EE:01", BTName: "RG-MLP80B", Model: m, ModelID: "RG-MLP80B", Status: discovery.Supported},
			}, nil
		}}
	go func() {
		defer close(d.done)
		d.path, d.err = try.Run(context.Background(), o)
	}()
	d.answer("Which one is the printer to test?", "2")
	got := d.finish()
	out := d.output()
	for _, want := range []string{"1) Bluetooth AA:BB:CC:DD:EE:03 (Bluetooth device, not a known label printer)",
		"2) Bluetooth AA:BB:CC:DD:EE:01 RG-MLP80B", `this one is "RG-MLP80B"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "~HI") || strings.Contains(out, "Zebra)") || strings.Contains(got, "~HI") {
		t.Errorf("treated like a Zebra:\n%s", out)
	}
}
