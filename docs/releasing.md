# Releasing (maintainers)

Push a tag `vX.Y.Z`; `.github/workflows/release.yml` does the rest in three jobs.

1. **`release`** (ubuntu, `.goreleaser.yaml`): runs the tests, builds the Linux archives, the `.deb`
   packages, the Docker images (including `:latest`) and `checksums.txt`, and creates a **draft** GitHub release.
2. **`release-windows`** (ubuntu, `.goreleaser-windows.yaml`, needs `release`): builds `pdb-print.exe` and
   `pdb-printw.exe` and the zip and uploads them with `checksums-windows.txt` to that draft. Then
   `scripts/windows-setup.sh` builds `pdb-print_X.Y.Z_windows_amd64_setup.exe` from the same exes with Inno
   Setup under Wine on the runner (the Inno installer pinned by version and SHA-256 in the script;
   `packaging/windows/pdb-print.iss`), adds it to `checksums-windows.txt`, and uploads both (`--clobber`).
   Inno's setup needs Wine's 32-bit side, which doesn't run inside `docker build`, and Wine doesn't run
   under Rosetta: the setup.exe part needs x86-64 Linux with Wine (the script's header lists the
   packages). PR CI (`packaging-windows`) runs it on every PR.
3. **`release-darwin`** (macos-latest, `.goreleaser-darwin.yaml`, needs both): builds the universal
   `pdb-print` (no cgo) and the Bluetooth helper `pdb-print-ble` (cgo, embedded Info.plist), uploads the
   macOS archive and `checksums-darwin.txt` to that draft (GoReleaser leaves it a draft: `draft: true`) and
   pushes the Homebrew cask to `francisrath/homebrew-tap`. Its last step, **publish the draft**, checks that
   the tag has exactly one release, that it is a draft, and that it carries the three checksum files, the
   Linux, Windows and macOS archives and the setup.exe; only then it publishes it. The step lists those files
   by name: renaming or adding an asset in a `.goreleaser*.yaml` or `scripts/windows-setup.sh` means updating
   that list too.

   Before the archive is made, `scripts/macos-sign.sh` (a GoReleaser hook on each universal binary) signs
   both binaries with the Developer ID certificate and the hardened runtime and notarises them; the job then
   checks the archive's binaries with `codesign --verify --strict` and `spctl --assess` before publishing.

A failed Windows or macOS build therefore leaves a draft, never a release without them. Between the cask push and the
publish step (seconds), the cask points at the still-draft release: `brew install` fails in that window.

## macOS signing

The `release-darwin` job needs these Actions secrets on `francisrath/pdb-print` and fails without them
(a release is never unsigned):

| Secret | What |
|---|---|
| `MACOS_SIGN_P12` | base64 of a `.p12` with the "Developer ID Application: Francis Rath (D4X9292QC4)" certificate, its private key and the Developer ID G2 intermediate |
| `MACOS_SIGN_PASSWORD` | the `.p12` password |
| `MACOS_NOTARY_ISSUER_ID` | App Store Connect → Users and Access → Integrations → Team Keys: Issuer ID |
| `MACOS_NOTARY_KEY_ID` | the key ID of the team key `pdb-print-notary` (role Developer) |
| `MACOS_NOTARY_KEY` | that key's `.p8` file contents |

No local copies are kept: if one is lost, make a new one and replace the secrets. Binaries signed before
stay valid, and macOS keeps the Bluetooth permission (it is tied to the bundle id and Team ID, not to one
certificate).

**The certificate expires on 2027-12-21.** Renew it before then:

1. `openssl genrsa -out devid.key 2048` and `openssl req -new -key devid.key -out devid.csr -subj "/CN=Francis Rath/C=NO"`.
2. developer.apple.com → Certificates → **+** → Developer ID Application (G2 Sub-CA) → upload the CSR →
   download `developerID_application.cer`.
3. Build the `.p12` with algorithms macOS's `security import` reads (OpenSSL 3's defaults fail with "MAC
   verification failed"):
   ```bash
   openssl x509 -inform der -in developerID_application.cer -out devid.pem
   curl -fsSO https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
   openssl x509 -inform der -in DeveloperIDG2CA.cer -out g2.pem
   openssl rand -base64 24 | tr -d '\n' >p12.pass
   openssl pkcs12 -export -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 \
     -inkey devid.key -in devid.pem -certfile g2.pem -passout file:p12.pass -out devid.p12
   ```
4. `base64 -i devid.p12 | tr -d '\n' | gh secret set MACOS_SIGN_P12` and `gh secret set MACOS_SIGN_PASSWORD <p12.pass`,
   then delete the local files. Let the old certificate expire; revoke it only if its key leaks (revoking stops
   every release signed with it from launching).

The notary key doesn't expire; revoke it in App Store Connect if it leaks and create a new one
(`gh secret set MACOS_NOTARY_KEY <AuthKey_ID.p8`, `MACOS_NOTARY_KEY_ID`).

Test a change to signing with a pre-release tag (e.g. `v0.2.1-rc.1`): it runs the whole release, but the tap
isn't updated (`skip_upload: auto`). PR CI (`packaging-darwin`) has no secrets and signs ad hoc instead.

## Recovery

- **`release-windows` fails:** `release-darwin` doesn't start and the release stays a draft. Fix the cause
  and re-run the failed jobs (GitHub re-runs `release-darwin` after it); `release-windows` overwrites its own
  uploads. It refuses to run without the draft, so after a published release turn it back into a draft
  first (`gh release edit TAG --draft=true`), then re-run both.
- **`release-darwin` fails:** the release stays a draft. Fix the cause and re-run that job; it finds the draft.
- **Only the publish step fails** (a check or the `gh` call): the cask already points at the draft. Look at the
  draft's assets and, if they are complete (and `checksums-windows.txt` lists the setup.exe, not only the zip),
  publish by hand: `gh release edit TAG --draft=false`; or re-run the job. If it reports several releases for the tag, delete the extra draft (below) first.
  The Docker images, `:latest` included, are already pushed by then (accepted trade-off).
- **Re-running `release-darwin` after the release was published** turns it back into a draft for the
  duration of the run. If that re-run fails, publish the draft by hand: `gh release edit TAG --draft=false`.
  Should GoReleaser fail to find the draft and make a second one, the publish step stops at "2 releases".
- **Re-running the Linux job** reuses the draft while it is still a draft. `release-darwin` refuses to run
  without that draft, so it never publishes a macOS-only release.
- **Re-running the Linux job after the release was published is unsupported:** GoReleaser finds no draft and
  creates a second, draft release for the tag. Delete that extra draft by hand (the published release stays):
  `gh api repos/francisrath/pdb-print/releases --jq '.[] | select(.draft and .tag_name == "TAG") | .id'`,
  then `gh api -X DELETE repos/francisrath/pdb-print/releases/ID`.
- **Don't re-run the release jobs of an old tag:** it can move the "latest" badge back to it.

Local macOS dry run: `go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean --config .goreleaser-darwin.yaml --skip=homebrew`

Local Windows dry run (setup.exe only on x86-64 Linux with Wine): `go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean --config .goreleaser-windows.yaml`, then `scripts/windows-setup.sh`

## Don't move a release tag

Never force-push or re-create a tag that has a release. GitHub turns the published release back into a
draft, and the tag push starts `release.yml` again, which builds into a second draft and replaces the
macOS archive and the Homebrew cask in the first one. That happened with v0.3.1 after a history
rewrite: the fix was to delete the extra draft and publish the original again
(`gh api -X PATCH repos/francisrath/pdb-print/releases/ID -F draft=false -f make_latest=true`), after
checking that its assets and the cask's checksum agree.

## New container packages

A new image name (`ghcr.io/francisrath/pdb-print` from the release after v0.3.1) gets a new package on
its first push. Check it on GitHub (Packages) and make it **public** if it isn't, or `docker pull` fails
with 401 for users. `pdb-print-stats` stays private: only srv1 pulls it.
