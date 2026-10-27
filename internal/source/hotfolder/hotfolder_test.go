package hotfolder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type harness struct {
	t       *testing.T
	f       *Folder
	state   map[string]*seen
	now     time.Time
	printed []string
	fail    bool
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, state: map[string]*seen{}, now: time.Now()}
	h.f = &Folder{Dir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Print: func(_ context.Context, path string) error {
			h.printed = append(h.printed, filepath.Base(path))
			if h.fail {
				return errors.New("printer on fire")
			}
			return nil
		}}
	h.f.defaults()
	return h
}

// file writes name with content, modified age ago (relative to the harness clock).
func (h *harness) file(name, content string, age time.Duration) string {
	p := filepath.Join(h.f.Dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	mt := h.now.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// scan advances the clock by d, then scans.
func (h *harness) scan(d time.Duration) {
	h.now = h.now.Add(d)
	h.f.scan(context.Background(), h.state, h.now)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPrintsFreshStableFileOnce(t *testing.T) {
	h := newHarness(t)
	h.file("label_Part32_bag.pdf", "%PDF-1", 2*time.Second)
	h.file("notes.pdf", "%PDF-1", 0) // doesn't match the pattern
	h.scan(0)                        // first sight
	h.scan(100 * time.Millisecond)   // unchanged: settle timer starts
	if len(h.printed) != 0 {
		t.Fatal("printed before settling")
	}
	h.scan(600 * time.Millisecond)
	if len(h.printed) != 1 || h.printed[0] != "label_Part32_bag.pdf" {
		t.Fatalf("printed %v", h.printed)
	}
	if !exists(filepath.Join(h.f.Dir, "printed", "label_Part32_bag.pdf")) {
		t.Error("not moved to printed/")
	}
	h.scan(time.Second)
	h.scan(time.Second)
	if len(h.printed) != 1 {
		t.Errorf("printed again: %v", h.printed)
	}
}

func TestIgnoresOldFiles(t *testing.T) {
	h := newHarness(t)
	old := h.file("label_Part1_bag.pdf", "%PDF-1", 5*time.Minute) // e.g. already there at startup
	for i := 0; i < 5; i++ {
		h.scan(time.Second)
	}
	if len(h.printed) != 0 || !exists(old) {
		t.Errorf("old file printed or moved: %v", h.printed)
	}
	// a fresh download replacing it is printed
	h.file("label_Part1_bag.pdf", "%PDF-1 new", 0)
	h.scan(0)
	h.scan(100 * time.Millisecond)
	h.scan(600 * time.Millisecond)
	if len(h.printed) != 1 {
		t.Errorf("replacement not printed: %v", h.printed)
	}
}

func TestWaitsForWritesToFinish(t *testing.T) {
	h := newHarness(t)
	h.file("label_Part2_bag.pdf", "%PDF", 0)
	h.scan(0)
	h.scan(time.Second)
	h.file("label_Part2_bag.pdf", "%PDF-1.4 more", 0) // still downloading
	h.scan(time.Second)
	if len(h.printed) != 0 {
		t.Fatal("printed while the file was still changing")
	}
	h.scan(100 * time.Millisecond)
	h.scan(600 * time.Millisecond)
	if len(h.printed) != 1 {
		t.Errorf("not printed after settling: %v", h.printed)
	}
}

func TestFailedPrintMovesToFailed(t *testing.T) {
	h := newHarness(t)
	h.fail = true
	h.file("label_Part3_bag.pdf", "%PDF-1", 0)
	h.scan(0)
	h.scan(100 * time.Millisecond)
	h.scan(600 * time.Millisecond)
	if !exists(filepath.Join(h.f.Dir, "failed", "label_Part3_bag.pdf")) {
		t.Error("not moved to failed/")
	}
	h.scan(time.Second)
	if len(h.printed) != 1 {
		t.Errorf("retried: %v", h.printed)
	}
}

func TestNeverReprintsWhenItCantMove(t *testing.T) {
	h := newHarness(t)
	// a plain file named "printed" makes the move impossible
	if err := os.WriteFile(filepath.Join(h.f.Dir, "printed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := h.file("label_Part4_bag.pdf", "%PDF-1", 0)
	h.scan(0)
	h.scan(100 * time.Millisecond)
	h.scan(600 * time.Millisecond)
	h.scan(time.Second)
	h.scan(time.Second)
	if len(h.printed) != 1 {
		t.Errorf("printed %d times", len(h.printed))
	}
	if exists(p) {
		t.Error("file left in place (should be deleted when it can't be moved)")
	}
}
