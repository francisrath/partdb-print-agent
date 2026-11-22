// Package selfupdate replaces the Windows exes of an installed pdb-print with a release's: it downloads
// the zip, checks its SHA-256 against the release's checksums file and swaps the files while no job runs.
package selfupdate

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultBase = "https://github.com/francisrath/pdb-print/releases/download"
	maxZip      = 200 << 20
)

// exes are the only files taken from the zip.
var exes = []string{"pdb-print.exe", "pdb-printw.exe"}

type Updater struct {
	Base    string       // "" = the GitHub releases download URL
	Client  *http.Client // nil = 5 min timeout
	Dir     string       // program dir holding pdb-print.exe and pdb-printw.exe
	Quiesce func(ctx context.Context) (release func(), err error)
	Restart func() error // per-user: spawn the new pdb-printw serve; service: os.Exit(3)
}

// Apply installs release version: download, verify, extract outside Dir, then swap under Quiesce.
// Restart runs once, after a successful swap.
func (u *Updater) Apply(ctx context.Context, version string) error {
	base := u.Base
	if base == "" {
		base = defaultBase
	}
	zipName := fmt.Sprintf("pdb-print_%s_windows_amd64.zip", version)
	zipURL := fmt.Sprintf("%s/v%s/%s", base, version, zipName)

	zf, err := os.CreateTemp("", "pdb-print-update-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(zf.Name())
	defer zf.Close()
	got, err := u.download(ctx, zipURL, zf)
	if err != nil {
		return err
	}
	sums, err := u.fetch(ctx, fmt.Sprintf("%s/v%s/checksums-windows.txt", base, version))
	if err != nil {
		return err
	}
	want := checksumFor(sums, zipName)
	if want == "" {
		return fmt.Errorf("checksums-windows.txt has no entry for %s", zipName)
	}
	if got != want {
		return fmt.Errorf("%s: checksum mismatch (got %s, want %s)", zipName, got, want)
	}

	tmp, err := os.MkdirTemp("", "pdb-print-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extract(zf.Name(), tmp); err != nil {
		return err
	}

	release := func() {}
	if u.Quiesce != nil {
		if release, err = u.Quiesce(ctx); err != nil {
			return fmt.Errorf("waiting for running jobs: %w", err)
		}
	}
	err = u.swap(tmp)
	release()
	if err != nil {
		return err
	}
	return u.Restart()
}

// swap renames each exe to .old and moves the new one in; on an error it puts the originals back.
func (u *Updater) swap(from string) (err error) {
	var done []string
	defer func() {
		if err == nil {
			return
		}
		for _, n := range done {
			p := filepath.Join(u.Dir, n)
			_ = os.Remove(p)
			_ = os.Rename(p+".old", p)
		}
	}()
	for _, n := range exes {
		p := filepath.Join(u.Dir, n)
		_ = os.Remove(p + ".old") // a stale one from an earlier update
		if err = os.Rename(p, p+".old"); err != nil {
			return err
		}
		done = append(done, n)
		if err = move(filepath.Join(from, n), p); err != nil {
			return err
		}
	}
	return nil
}

// move renames, or copies then removes when src is on another volume. A failed copy leaves nothing at dst.
func move(src, dst string) error {
	if os.Rename(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Close()
	} else {
		_ = out.Close()
	}
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	_ = in.Close()
	_ = os.Remove(src)
	return nil
}

func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	c := u.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// download writes the body to w and returns its SHA-256 (hex).
func (u *Updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxZip+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxZip {
		return "", fmt.Errorf("download %s: larger than %d MB", url, maxZip>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *Updater) fetch(ctx context.Context, url string) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// checksumFor finds name's hash in sha256sum output ("<hex>  <name>", name possibly prefixed with *).
func checksumFor(sums, name string) string {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// extract writes the exes from the zip into dir; any other entry is ignored.
func extract(zipPath, dir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open update zip: %w", err)
	}
	defer zr.Close()
	for _, n := range exes {
		var f *zip.File
		for _, e := range zr.File {
			if e.Name == n {
				f = e
			}
		}
		if f == nil {
			return fmt.Errorf("update zip has no %s", n)
		}
		if err := extractOne(f, filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

func extractOne(f *zip.File, dst string) error {
	if f.UncompressedSize64 > maxZip {
		return fmt.Errorf("%s in the update zip is too large", f.Name)
	}
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, io.LimitReader(in, maxZip)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// CleanupOld deletes the *.old files an update left in dir; call it once the new version has started.
func CleanupOld(dir string) {
	old, _ := filepath.Glob(filepath.Join(dir, "*.old"))
	for _, p := range old {
		_ = os.Remove(p) // still in use: the next start tries again
	}
}
