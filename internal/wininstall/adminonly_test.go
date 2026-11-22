package wininstall

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteAdminOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pair-code.txt")
	for _, data := range []string{"123456", "654321"} { // the second replaces the first
		if err := WriteAdminOnly(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != data { // the test runs as the owner (or admin)
			t.Fatalf("read %q %v, want %q", b, err, data)
		}
	}
	if fi, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
