// Package hotfolder prints label PDFs that appear in a folder (architecture §4.1, source 3):
// Part-DB's label downloads are named label_<Type><id>_<profile>.pdf.
package hotfolder

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		if ctx.Err() != nil {
			return // shutting down: leave the remaining files in place
		}
		if now.Sub(fi.ModTime()) > f.MaxAge {
			s.skipped = true // settled too late to count as a fresh download
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
	printErr := f.Print(ctx, path)
	dest := "printed"
	if printErr != nil {
		f.Log.Error("hot folder: print failed", "file", filepath.Base(path), "err", printErr)
		dest = "failed"
	} else {
		f.Log.Info("hot folder: printed", "file", filepath.Base(path))
	}
	dir := filepath.Join(f.Dir, dest)
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		err = moveNoClobber(path, dir)
	}
	switch {
	case err == nil:
	case printErr != nil:
		// keep the PDF that didn't print; it stays marked, so it isn't retried
		f.Log.Error("hot folder: can't move to failed/, left in place", "file", path, "err", err)
	default:
		// printed but can't be moved: delete rather than risk printing it again later
		if err := os.Remove(path); err != nil {
			f.Log.Error("hot folder: can't move or delete", "file", path, "err", err)
		}
	}
}

const maxNumbered = 1000

// moveNoClobber moves src into dir without ever replacing a file there: the same label
// downloaded twice is archived as "name.pdf" and "name (2).pdf". It never gives up:
// past maxNumbered it uses a unique timestamped name.
func moveNoClobber(src, dir string) error {
	base := filepath.Base(src)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		name := base
		switch {
		case i > maxNumbered:
			name = fmt.Sprintf("%s (%d)%s", stem, time.Now().UnixNano(), ext)
		case i > 1:
			name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		placed, err := placeExclusive(src, filepath.Join(dir, name))
		if err != nil || placed {
			return err
		}
	}
}

// placeExclusive moves src to dst unless dst exists (then false). Both ways are atomic
// with respect to dst: a hard link, or on filesystems without links (e.g. FAT) an
// exclusive create plus copy.
func placeExclusive(src, dst string) (bool, error) {
	err := os.Link(src, dst)
	switch {
	case err == nil:
		return true, os.Remove(src)
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	return copyExclusive(src, dst)
}

func copyExclusive(src, dst string) (bool, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err == nil {
		_, err = io.Copy(out, in)
		in.Close()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return false, err
	}
	return true, os.Remove(src)
}
