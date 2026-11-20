# Releasing (maintainers)

Push a tag `vX.Y.Z`; `.github/workflows/release.yml` does the rest in two jobs.

1. **`release`** (ubuntu, `.goreleaser.yaml`): runs the tests, builds Linux and Windows archives, the `.deb`
   packages, the Docker images (including `:latest`) and `checksums.txt`, and creates a **draft** GitHub release.
2. **`release-darwin`** (macos-latest, `.goreleaser-darwin.yaml`, needs `release`): builds the universal
   `pdb-print` (no cgo) and the Bluetooth helper `pdb-print-ble` (cgo, embedded Info.plist), uploads the
   macOS archive and `checksums-darwin.txt` to that draft, then GoReleaser publishes the release, then pushes
   the Homebrew cask to `francisrath/homebrew-tap`.

A failed macOS build therefore leaves a draft, never a release without macOS.

## Recovery

- **`release-darwin` fails:** the release stays a draft. Fix the cause and re-run that job; it finds the draft.
  The Docker images, `:latest` included, are already pushed by then (accepted trade-off).
- **Re-running `release-darwin` after the release was published** turns it back into a draft for the
  duration of the run. If that re-run fails, publish the draft by hand: `gh release edit TAG --draft=false`.
- **Re-running the Linux job** reuses the draft while it is still a draft. `release-darwin` refuses to run
  without that draft, so it never publishes a macOS-only release.
- **Re-running the Linux job after the release was published is unsupported:** GoReleaser finds no draft and
  creates a second, draft release for the tag. Delete that extra draft by hand (the published release stays):
  `gh api repos/francisrath/partdb-print-agent/releases --jq '.[] | select(.draft and .tag_name == "TAG") | .id'`,
  then `gh api -X DELETE repos/francisrath/partdb-print-agent/releases/ID`.
- **Don't re-run the release jobs of an old tag:** it can move the "latest" badge back to it.

Local macOS dry run: `go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean --config .goreleaser-darwin.yaml --skip=homebrew`
