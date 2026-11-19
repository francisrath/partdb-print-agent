package try_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/discovery"
	"github.com/francisrath/partdb-print-agent/internal/driver/zpl/zpltest"
	"github.com/francisrath/partdb-print-agent/internal/printing"
	"github.com/francisrath/partdb-print-agent/internal/try"
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
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
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
	case <-time.After(30 * time.Second):
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

func start(t *testing.T, addr string) *dialog {
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	r, w := io.Pipe()
	d := &dialog{t: t, in: w, done: make(chan struct{})}
	svc := &printing.Service{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(svc.Close)
	o := try.Options{In: r, Out: d, Port: p, Dir: t.TempDir(), Svc: svc, Version: "test",
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
	for _, q := range []string{"Did label 1/5 come out", "whole frame", "TOP at the top edge", "mirrored", "dark enough", "checkerboard",
		"sharp", "phone", "barcode scanner", "Did two labels come out"} {
		d.answer(q, "")
	}
	d.answer("Open the printer's lid", "", func() { fake.Set(func(f *zpltest.Fake) { f.HeadUp, f.Errors, f.SilentHS = true, 0x4, true }) })
	d.answer("Close the lid again", "", func() { fake.Set(func(f *zpltest.Fake) { f.HeadUp, f.Errors, f.SilentHS = false, 0, false }) })
	got := d.finish()

	for _, want := range []string{"Zebra GK420d", "Labels: 102×102 mm", "Label 1/5: printed", "Label 2/5: printed", "Label 3a+3b/5: printed",
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
	all := fake.AllFormats()
	if last := all[len(all)-1]; !strings.Contains(last, "^PW799^LL812^LH0,0^LS0") || strings.Contains(last, "^GF") {
		t.Errorf("last thing sent isn't the settings restore: %.100q", last)
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
	d.answer("Did label 1/5 come out", "")
	d.answer("whole frame", "")
	d.answer("TOP at the top edge", "n")
	d.answer("upside down", "y")
	d.answer("TOP at the top edge now", "y")
	got := d.finish()
	f := fake.Formats()
	if len(f) < 2 || f[0] == f[1] || !strings.Contains(got, "Label 1b/5: printed") || !strings.Contains(got, "upside_down=true") {
		t.Errorf("%d formats; report:\n%s", len(f), got)
	}
}

func TestTryShiftRetry(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did label 1/5 come out", "")
	d.answer("whole frame", "n")
	d.answer("Which side", "l")
	d.answer("how many mm", "3")
	d.answer("whole frame", "y")
	d.finish()
	if f := fake.Formats(); len(f) < 2 || !strings.Contains(f[1], "^LH24,0") {
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
	d.answer("Did label 1/5 come out", "y")
	got := d.finish()
	all := fake.AllFormats()
	if !strings.Contains(got, "Label 1/5: printed") || !strings.Contains(all[len(all)-1], "^PW799^LL812") {
		t.Errorf("report:\n%s\nlast sent %.80q", got, all[len(all)-1])
	}
}

// Enter at "Which side" skips the fix but not the questions after it.
func TestTrySkipSideContinues(t *testing.T) {
	fake := &zpltest.Fake{Hold: 600 * time.Millisecond}
	d := start(t, fake.Start(t))
	d.answer("Ask this printer what it is?", "y")
	d.answer("Number, or Enter for", "")
	d.answer("Print the test labels now?", "y")
	d.answer("Did label 1/5 come out", "")
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
	d.answer("Did label 1/5 come out", "")
	d.answer("whole frame", "n")
	d.answer("Which side", "b")
	d.finish() // closed at the mm question
	if n := len(fake.Formats()); n != 1 {
		t.Errorf("%d labels sent, want only 1/5", n)
	}
}
