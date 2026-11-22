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
	"sync"
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
func setup(t *testing.T) (*Updater, string, *int) {
	t.Helper()
	old := grace
	grace = 0
	t.Cleanup(func() { grace = old })
	dir := t.TempDir()
	for _, n := range []string{"pdb-print.exe", "pdb-printw.exe"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	restarts := new(int)
	return &Updater{Dir: dir,
		Quiesce: func(context.Context) (func(), error) { return func() {}, nil },
		Restart: func() error { *restarts++; return nil }}, dir, restarts
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
	u, dir, restarts := setup(t)
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
	u, dir, restarts := setup(t)
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
	u, dir, _ := setup(t)
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
	u, dir, restarts := setup(t)
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
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("dir has %v after rollback, want only pdb-print.exe", es)
	}
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "old" {
		t.Errorf("pdb-print.exe = %q after rollback, want old", got)
	}
	if *restarts != 0 {
		t.Error("Restart called after a failure")
	}
}

func TestApplyWaitsForPrint(t *testing.T) {
	u, dir, _ := setup(t)
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
	grace = 200 * time.Millisecond // a just-finished job's report still goes out
	close(printDone)
	time.Sleep(50 * time.Millisecond)
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "old" {
		t.Error("swapped before the grace was over")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "pdb-print.exe")); got != "new-cli" {
		t.Errorf("not swapped after the print: %q", got)
	}
}

func TestApplyQuiesceError(t *testing.T) {
	u, dir, restarts := setup(t)
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

func TestApplyNeedsQuiesceAndRestart(t *testing.T) {
	for _, drop := range []func(*Updater){func(u *Updater) { u.Quiesce = nil }, func(u *Updater) { u.Restart = nil }} {
		u, _, _ := setup(t)
		hit := false
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
		u.Base = srv.URL
		drop(u)
		if err := u.Apply(context.Background(), ver); err == nil {
			t.Error("Apply without Quiesce or Restart succeeded")
		}
		srv.Close()
		if hit {
			t.Error("downloaded before checking the callbacks")
		}
	}
}

// Jobs stay held back until Restart is done, so none is cut off by the exit.
func TestApplyRestartsWhileQuiesced(t *testing.T) {
	u, _, _ := setup(t)
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	var mu sync.RWMutex // stands in for printing.Service's gate
	u.Quiesce = func(context.Context) (func(), error) { mu.Lock(); return mu.Unlock, nil }
	started := make(chan struct{})
	u.Restart = func() error {
		go func() { mu.RLock(); close(started); mu.RUnlock() }() // a job arriving now
		select {
		case <-started:
			t.Error("a job started before Restart returned")
		case <-time.After(50 * time.Millisecond):
		}
		return errors.New("restart failed")
	}
	if err := u.Apply(context.Background(), ver); err == nil || !errors.Is(err, ErrInstalled) {
		t.Fatalf("Restart's error: %v, want one that is ErrInstalled", err)
	}
	select {
	case <-started: // released after Restart returned an error
	case <-time.After(2 * time.Second):
		t.Fatal("jobs not released after a failed Restart")
	}
}

func TestApplyLeavesNoTempFiles(t *testing.T) {
	for _, bad := range []bool{true, false} {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		t.Setenv("TMP", tmp)
		t.Setenv("TEMP", tmp)
		u, _, _ := setup(t)
		sumArg := ""
		if bad {
			sumArg = sum([]byte("other"))
		}
		srv := httptest.NewServer(good(t, sumArg))
		u.Base = srv.URL
		err := u.Apply(context.Background(), ver)
		srv.Close()
		if (err != nil) != bad {
			t.Fatalf("bad=%v: err %v", bad, err)
		}
		if es, _ := os.ReadDir(tmp); len(es) != 0 {
			t.Errorf("bad=%v: temp dir holds %v", bad, es)
		}
	}
}

func TestApplyFailedCopyRestoresOriginals(t *testing.T) {
	u, dir, restarts := setup(t)
	srv := httptest.NewServer(good(t, ""))
	defer srv.Close()
	u.Base = srv.URL
	u.create = func(p string) (*os.File, error) {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err == nil && filepath.Base(p) == "pdb-printw.exe" {
			f.Close() // writing to it fails
		}
		return f, err
	}
	if err := u.Apply(context.Background(), ver); err == nil {
		t.Fatal("a failed copy was reported as success")
	}
	untouched(t, dir)
	if *restarts != 0 {
		t.Error("Restart called after a failure")
	}
}
