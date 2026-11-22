package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const ver = "1.2.3"

func fakeZip(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range map[string]string{"pdb-print.exe": "new-cli", "pdb-printw.exe": "new-gui", "LICENSE": "mit"} {
		f, _ := w.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// setup serves zip and checksums under /v1.2.3/ and returns an Updater over a dir holding old exes.
func setup(t *testing.T, serve http.HandlerFunc) (*Updater, string, *int) {
	t.Helper()
	srv := httptest.NewServer(serve)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	for _, n := range []string{"pdb-print.exe", "pdb-printw.exe"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	restarts := new(int)
	return &Updater{Base: srv.URL, Dir: dir, Restart: func() error { *restarts++; return nil }}, dir, restarts
}

func good(t *testing.T, checksum string) http.HandlerFunc {
	zb := fakeZip(t)
	name := fmt.Sprintf("pdb-print_%s_windows_amd64.zip", ver)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v" + ver + "/" + name:
			_, _ = w.Write(zb)
		case "/v" + ver + "/checksums-windows.txt":
			if checksum == "" {
				checksum = sum(zb)
			}
			fmt.Fprintf(w, "%s  other.zip\n%s  %s\n", sum(nil), checksum, name)
		default:
			http.NotFound(w, r)
		}
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplySwaps(t *testing.T) {
	u, dir, restarts := setup(t, nil)
	u.Client = nil
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	if err := u.Apply(context.Background(), ver); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"pdb-print.exe": "new-cli", "pdb-printw.exe": "new-gui", "pdb-print.exe.old": "old", "pdb-printw.exe.old": "old"} {
		if got := read(t, filepath.Join(dir, n)); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
	if *restarts != 1 {
		t.Errorf("Restart called %d times, want 1", *restarts)
	}
	CleanupOld(dir)
	if _, err := os.Stat(filepath.Join(dir, "pdb-print.exe.old")); !os.IsNotExist(err) {
		t.Error(".old left after CleanupOld")
	}
	if read(t, filepath.Join(dir, "pdb-print.exe")) != "new-cli" {
		t.Error("CleanupOld removed the live exe")
	}
}

func untouched(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	if len(es) != 2 {
		t.Errorf("dir has %d entries, want just the 2 exes: %v", len(es), es)
	}
	for _, n := range []string{"pdb-print.exe", "pdb-printw.exe"} {
		if got := read(t, filepath.Join(dir, n)); got != "old" {
			t.Errorf("%s = %q, want untouched", n, got)
		}
	}
}

func TestApplyBadChecksum(t *testing.T) {
	u, dir, restarts := setup(t, nil)
	srv := httptest.NewServer(good(t, sum([]byte("other"))))
	defer srv.Close()
	u.Base = srv.URL
	if err := u.Apply(context.Background(), ver); err == nil {
		t.Fatal("bad checksum accepted")
	}
	untouched(t, dir)
	if *restarts != 0 {
		t.Error("Restart called after a failure")
	}
}

func TestApplyInterruptedDownload(t *testing.T) {
	u, dir, _ := setup(t, nil)
	zb := fakeZip(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(zb)))
		_, _ = w.Write(zb[:len(zb)/2])
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	u.Base = srv.URL
	if err := u.Apply(context.Background(), ver); err == nil {
		t.Fatal("truncated download accepted")
	}
	untouched(t, dir)
}

func TestApplyRollsBack(t *testing.T) {
	u, dir, restarts := setup(t, nil)
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	// the second exe's move fails: its target is gone, so the first swap must be undone
	if err := os.Remove(filepath.Join(dir, "pdb-printw.exe")); err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(context.Background(), ver); err == nil {
		t.Fatal("swap of a missing exe succeeded")
	}
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "old" {
		t.Errorf("pdb-print.exe = %q after rollback, want old", got)
	}
	if *restarts != 0 {
		t.Error("Restart called after a failure")
	}
}

func TestApplyWaitsForPrint(t *testing.T) {
	u, dir, _ := setup(t, nil)
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	printDone := make(chan struct{})
	entered := make(chan struct{})
	u.Quiesce = func(context.Context) (func(), error) {
		close(entered)
		<-printDone // a print is running
		return func() {}, nil
	}
	done := make(chan error, 1)
	go func() { done <- u.Apply(context.Background(), ver) }()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("Apply returned before the print ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "old" {
		t.Fatal("swapped while a print was running")
	}
	close(printDone)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "new-cli" {
		t.Errorf("not swapped after the print: %q", got)
	}
}

func TestApplyQuiesceError(t *testing.T) {
	u, dir, restarts := setup(t, nil)
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	u.Quiesce = func(context.Context) (func(), error) { return nil, errors.New("busy") }
	if err := u.Apply(context.Background(), ver); err == nil {
		t.Fatal("expected error")
	}
	untouched(t, dir)
	if *restarts != 0 {
		t.Error("Restart called after a failure")
	}
}
