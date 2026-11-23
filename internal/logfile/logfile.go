// Package logfile is a log file that rotates itself: past Max bytes it is renamed to
// <path>.1 (replacing the previous one) and a new file is started.
package logfile

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
)

// renameBackoff: after a failed rotation, appending goes on and the next try waits this long.
const renameBackoff = time.Minute

// Writer appends to Path, created (with its folder) on the first Write.
type Writer struct {
	Path string
	Max  int64 // rotate before a write would take the file past this size

	mu      sync.Mutex
	f       *os.File
	size    int64
	crashes bool      // the runtime's crash output goes to f too
	retry   time.Time // no rotation before this, after a failed rename
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.Max && time.Now().After(w.retry) {
		w.release()
		// replaces the old generation; if it fails (file held open elsewhere), keep appending
		if os.Rename(w.Path, w.Path+".1") != nil {
			w.retry = time.Now().Add(renameBackoff)
		}
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
	if w.crashes {
		return debug.SetCrashOutput(f, debug.CrashOptions{})
	}
	return nil
}

// release closes f, and the runtime's copy of it: an open file can't be renamed on Windows.
func (w *Writer) release() {
	if w.crashes {
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	}
	w.f.Close()
	w.f = nil
}

// KeepCrashes opens the file now and sends the runtime's crash output (panics, fatal errors) to it,
// also after each rotation.
func (w *Writer) KeepCrashes() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.crashes = true
	if w.f != nil {
		return debug.SetCrashOutput(w.f, debug.CrashOptions{})
	}
	return w.open()
}

// Close closes the file; a later Write opens it again.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.release()
	}
	return nil
}
