package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "pdb-print.log")
	w := &Writer{Path: p, Max: 10}
	defer w.Close()
	for _, s := range []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n", "eeee\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	// 10 bytes fit; the third line rotates, the fifth rotates again and drops the first generation
	if got, got1 := read(t, p), read(t, p+".1"); got != "eeee\n" || got1 != "cccc\ndddd\n" {
		t.Errorf("log %q, .1 %q", got, got1)
	}
}

func TestAppendsAndRotatesExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pdb-print.log")
	os.WriteFile(p, []byte("0123456789"), 0o644) // from an earlier run, already full
	w := &Writer{Path: p, Max: 10}
	defer w.Close()
	w.Write([]byte("x\n"))
	if got, got1 := read(t, p), read(t, p+".1"); got != "x\n" || got1 != "0123456789" {
		t.Errorf("log %q, .1 %q", got, got1)
	}
}

func TestLongLineIsWrittenWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pdb-print.log")
	w := &Writer{Path: p, Max: 4}
	defer w.Close()
	w.Write([]byte("longer than max\n"))
	if got := read(t, p); got != "longer than max\n" {
		t.Errorf("log %q", got)
	}
}

// A rotation that can't rename (on Windows: the file is open elsewhere) keeps appending and
// doesn't try again on every write.
func TestFailedRenameBacksOff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pdb-print.log")
	os.MkdirAll(filepath.Join(p+".1", "x"), 0o755) // a non-empty directory: the rename fails
	w := &Writer{Path: p, Max: 4}
	defer w.Close()
	w.Write([]byte("aaaa"))
	w.Write([]byte("bbbb")) // rotation fails
	os.RemoveAll(p + ".1")
	w.Write([]byte("cccc")) // within the backoff: no new try
	if got := read(t, p); got != "aaaabbbbcccc" || exists(p+".1") {
		t.Errorf("log %q, .1 exists %v", got, exists(p+".1"))
	}
	w.retry = time.Time{} // the minute is up
	w.Write([]byte("dddd"))
	if got, got1 := read(t, p), read(t, p+".1"); got != "dddd" || got1 != "aaaabbbbcccc" {
		t.Errorf("log %q, .1 %q", got, got1)
	}
}

func TestKeepCrashesOpensNow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pdb-print.log")
	w := &Writer{Path: p, Max: 4}
	if err := w.KeepCrashes(); err != nil {
		t.Fatal(err)
	}
	if !exists(p) {
		t.Error("not opened")
	}
	w.Write([]byte("aaaa"))
	w.Write([]byte("bbbb")) // rotates with crash output on
	if got, got1 := read(t, p), read(t, p+".1"); got != "bbbb" || got1 != "aaaa" {
		t.Errorf("log %q, .1 %q", got, got1)
	}
	w.Close() // also resets the crash output for the rest of the tests
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
