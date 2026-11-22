// Package logfile is a log file that rotates itself: past Max bytes it is renamed to
// <path>.1 (replacing the previous one) and a new file is started.
package logfile

import (
	"os"
	"path/filepath"
	"sync"
)

// Writer appends to Path, created (with its folder) on the first Write.
type Writer struct {
	Path string
	Max  int64 // rotate before a write would take the file past this size

	mu   sync.Mutex
	f    *os.File
	size int64
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.Max {
		w.f.Close()
		w.f = nil
		_ = os.Rename(w.Path, w.Path+".1") // replaces the old generation; if it fails, keep appending
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) open() error {
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Close closes the file; a later Write opens it again.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
