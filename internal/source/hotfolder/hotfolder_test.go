package hotfolder

import (
	"context"
	"errors"
	"fmt"
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

func TestSameNameTwiceKeepsBoth(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2; i++ {
		h.file("label_Part5_bag.pdf", fmt.Sprintf("%%PDF-1 copy %d", i), 0)
		h.scan(0)
		h.scan(100 * time.Millisecond)
		h.scan(600 * time.Millisecond)
		h.scan(time.Second) // gone from the folder: forget it
	}
	for _, name := range []string{"label_Part5_bag.pdf", "label_Part5_bag (2).pdf"} {
		if !exists(filepath.Join(h.f.Dir, "printed", name)) {
			t.Errorf("printed/%s missing", name)
		}
	}
	if len(h.printed) != 2 {
		t.Errorf("printed %v", h.printed)
	}
}

func TestKeepsFailedPDFWhenItCantMove(t *testing.T) {
	h := newHarness(t)
	h.fail = true
	if err := os.WriteFile(filepath.Join(h.f.Dir, "failed"), nil, 0o600); err != nil { // blocks failed/
		t.Fatal(err)
	}
	p := h.file("label_Part6_bag.pdf", "%PDF-1", 0)
	h.scan(0)
	h.scan(100 * time.Millisecond)
	h.scan(600 * time.Millisecond)
	h.scan(time.Second)
	if !exists(p) || len(h.printed) != 1 {
		t.Errorf("failed PDF lost or retried: exists=%v printed=%v", exists(p), h.printed)
	}
}

func TestSkipsFileThatSettlesTooLate(t *testing.T) {
	h := newHarness(t)
	h.file("label_Part7_bag.pdf", "%PDF-1", 59*time.Second) // fresh at first sight
	h.scan(0)
	h.scan(100 * time.Millisecond)
	h.scan(2 * time.Second) // now 61 s old when it would be printed
	if len(h.printed) != 0 {
		t.Errorf("printed a file older than MaxAge: %v", h.printed)
	}
}

func TestStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	h.file("label_Part8_bag.pdf", "%PDF-1", 0)
	h.scan(0)
	h.scan(100 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.now = h.now.Add(600 * time.Millisecond)
	h.f.scan(ctx, h.state, h.now)
	if len(h.printed) != 0 {
		t.Errorf("printed after cancel: %v", h.printed)
	}
}

func TestMoveNeverGivesUp(t *testing.T) {
	src, dir := t.TempDir(), t.TempDir()
	for i := 1; i <= maxNumbered; i++ {
		name := "label_X.pdf"
		if i > 1 {
			name = fmt.Sprintf("label_X (%d).pdf", i)
		}
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(src, "label_X.pdf")
	if err := os.WriteFile(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveNoClobber(p, dir); err != nil || exists(p) {
		t.Fatalf("move: %v, source left: %v", err, exists(p))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != maxNumbered+1 {
		t.Errorf("%d files, want %d (nothing overwritten)", len(entries), maxNumbered+1)
	}
}

func TestCopyExclusiveNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.pdf"), filepath.Join(dir, "b.pdf")
	os.WriteFile(src, []byte("new"), 0o600)
	os.WriteFile(dst, []byte("old"), 0o600)
	if placed, err := copyExclusive(src, dst); placed || err != nil {
		t.Fatalf("existing dst: placed=%v err=%v", placed, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" || !exists(src) {
		t.Fatal("overwrote the destination or lost the source")
	}
	os.Remove(dst)
	if placed, err := copyExclusive(src, dst); !placed || err != nil {
		t.Fatalf("free dst: placed=%v err=%v", placed, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" || exists(src) {
		t.Error("copy wrong or source not removed")
	}
}
