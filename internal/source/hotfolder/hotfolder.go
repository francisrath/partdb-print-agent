// Package hotfolder prints label PDFs that appear in a folder (architecture §4.1, source 3):
// Part-DB's label downloads are named label_<Type><id>_<profile>.pdf.
package hotfolder

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Folder polls Dir for new files matching Pattern. A file is printed once it is younger
// than MaxAge and its size and mtime have been stable for Settle; afterwards it moves to
// Dir/printed (or Dir/failed when printing fails), so nothing is printed twice.
// ponytail: polling instead of fsnotify — no dependency, same behaviour on every OS;
// one ReadDir per second is negligible.
type Folder struct {
	Dir     string
	Pattern string        // default "label_*.pdf"
	MaxAge  time.Duration // default 60 s: older files (e.g. at startup) are never printed
	Settle  time.Duration // default 500 ms
	Poll    time.Duration // default 1 s
	Print   func(ctx context.Context, path string) error
	Log     *slog.Logger
}

type seen struct {
	size    int64
	mod     time.Time
	stable  time.Time // when size/mtime were first observed unchanged
	skipped bool      // too old: never print
}

func (f *Folder) defaults() {
	if f.Pattern == "" {
		f.Pattern = "label_*.pdf"
	}
	if f.MaxAge == 0 {
		f.MaxAge = 60 * time.Second
	}
	if f.Settle == 0 {
		f.Settle = 500 * time.Millisecond
	}
	if f.Poll == 0 {
		f.Poll = time.Second
	}
}

// Run polls until ctx is done.
func (f *Folder) Run(ctx context.Context) error {
	f.defaults()
	f.Log.Info("hot folder: watching", "dir", f.Dir, "pattern", f.Pattern)
	state := map[string]*seen{}
	t := time.NewTicker(f.Poll)
	defer t.Stop()
	for {
		f.scan(ctx, state, time.Now())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (f *Folder) scan(ctx context.Context, state map[string]*seen, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(f.Dir, f.Pattern))
	if err != nil {
		f.Log.Error("hot folder: bad pattern", "pattern", f.Pattern, "err", err)
		return
	}
	present := map[string]bool{}
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		present[path] = true
		s := state[path]
		changed := s != nil && (fi.Size() != s.size || !fi.ModTime().Equal(s.mod))
		switch {
		case s == nil || changed: // new, still being written, or replaced by a new download
			state[path] = &seen{size: fi.Size(), mod: fi.ModTime(), skipped: now.Sub(fi.ModTime()) > f.MaxAge}
			continue
		case s.skipped:
			continue
		case s.stable.IsZero():
			s.stable = now
		}
		if now.Sub(s.stable) < f.Settle {
			continue
		}
		f.handle(ctx, path)
		s.skipped = true // done; if it couldn't be moved away, it isn't printed again unless it changes
	}
	for path := range state {
		if !present[path] {
			delete(state, path) // gone (moved, deleted, renamed)
		}
	}
}

func (f *Folder) handle(ctx context.Context, path string) {
	dest := "printed"
	if err := f.Print(ctx, path); err != nil {
		f.Log.Error("hot folder: print failed", "file", filepath.Base(path), "err", err)
		dest = "failed"
	} else {
		f.Log.Info("hot folder: printed", "file", filepath.Base(path))
	}
	dir := filepath.Join(f.Dir, dest)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := moveNoClobber(path, dir); err == nil {
			return
		}
	}
	// can't move it: delete rather than risk printing it again
	if err := os.Remove(path); err != nil {
		f.Log.Error("hot folder: can't move or delete", "file", path, "err", err)
	}
}

// moveNoClobber moves src into dir without ever replacing a file there: the same label
// downloaded twice is archived as "name.pdf" and "name (2).pdf".
func moveNoClobber(src, dir string) error {
	base := filepath.Base(src)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		dst := filepath.Join(dir, name)
		err := os.Link(src, dst) // fails if dst exists, so nothing is ever overwritten
		if err == nil {
			return os.Remove(src)
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		// no hard links on this filesystem (e.g. FAT): check, then rename
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		return os.Rename(src, dst)
	}
	return errors.New("no free file name")
}
