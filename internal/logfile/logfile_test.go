package logfile

import (
	"os"
	"path/filepath"
	"testing"
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
