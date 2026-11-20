# Releasing (maintainers)

Push a tag `vX.Y.Z`; `.github/workflows/release.yml` does the rest in two jobs.

1. **`release`** (ubuntu, `.goreleaser.yaml`): runs the tests, builds Linux and Windows archives, the `.deb`
   packages, the Docker images (including `:latest`) and `checksums.txt`, and creates a **draft** GitHub release.
2. **`release-darwin`** (macos-latest, `.goreleaser-darwin.yaml`, needs `release`): builds the universal
   `pdb-print` (no cgo) and the Bluetooth helper `pdb-print-ble` (cgo, embedded Info.plist), uploads the
   macOS archive and `checksums-darwin.txt` to that draft (GoReleaser leaves it a draft: `draft: true`) and
   pushes the Homebrew cask to `francisrath/homebrew-tap`. Its last step, **publish the draft**, checks that
   the tag has exactly one release, that it is a draft, and that it carries `checksums.txt`,
   `checksums-darwin.txt` and the Linux, Windows and macOS archives; only then it publishes it.

A failed macOS build therefore leaves a draft, never a release without macOS. Between the cask push and the
publish step (seconds), the cask points at the still-draft release: `brew install` fails in that window.

## Recovery

- **`release-darwin` fails:** the release stays a draft. Fix the cause and re-run that job; it finds the draft.
- **Only the publish step fails** (a check or the `gh` call): the cask already points at the draft. Look at the
  draft's assets and, if they are complete, publish by hand: `gh release edit TAG --draft=false`; or re-run
  the job. If it reports several releases for the tag, delete the extra draft (below) first.
  The Docker images, `:latest` included, are already pushed by then (accepted trade-off).
- **Re-running `release-darwin` after the release was published** turns it back into a draft for the
  duration of the run. If that re-run fails, publish the draft by hand: `gh release edit TAG --draft=false`.
  Should GoReleaser fail to find the draft and make a second one, the publish step stops at "2 releases".
- **Re-running the Linux job** reuses the draft while it is still a draft. `release-darwin` refuses to run
  without that draft, so it never publishes a macOS-only release.
- **Re-running the Linux job after the release was published is unsupported:** GoReleaser finds no draft and
  creates a second, draft release for the tag. Delete that extra draft by hand (the published release stays):
  `gh api repos/francisrath/partdb-print-agent/releases --jq '.[] | select(.draft and .tag_name == "TAG") | .id'`,
  then `gh api -X DELETE repos/francisrath/partdb-print-agent/releases/ID`.
- **Don't re-run the release jobs of an old tag:** it can move the "latest" badge back to it.

Local macOS dry run: `go run github.com/goreleaser/goreleaser/v2@v2.20.3 release --snapshot --clean --config .goreleaser-darwin.yaml --skip=homebrew`
