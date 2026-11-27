# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by
[SignPath Foundation](https://signpath.org).

This applies to the Windows files of pdb-print releases: `pdb-print.exe` and, once there is one, the
Windows installer. macOS builds are signed and notarized separately with the maintainer's Apple Developer
ID. Until SignPath Foundation has accepted the project, Windows releases are unsigned.

## What gets signed

Only files built from this repository's source by the release workflow
([`.github/workflows/release.yml`](../.github/workflows/release.yml)) on GitHub Actions, from a version
tag on `main`, are signed; the workflow refuses a tag that isn't on `main`, and each signing request is
approved by hand. Nothing built on a personal machine is signed, and no third-party programs are signed.

## Team and roles

| Role | Who |
|---|---|
| Committers and reviewers | [Repository collaborators](https://github.com/francisrath/pdb-print/graphs/contributors) with write access: [@francisrath](https://github.com/francisrath) |
| Approvers | [@francisrath](https://github.com/francisrath) (repository owner) |

- **Committers** may change the source without further review.
- **Reviewers:** every change from anyone without write access comes in as a pull request and is reviewed
  by a committer before it is merged.
- **Approvers:** each signing request is approved by hand, once per release, by an approver.

All team members use multi-factor authentication for GitHub and SignPath.

## Privacy

pdb-print only connects to systems you set up, plus two optional services. In detail:

- **Your Part-DB server**, at the URL you enter, to fetch labels and agent jobs.
- **Your printers**, over the network, USB, serial or Bluetooth, as configured.
- **GitHub**, when `pdb-print serve` starts and then once a day while it runs, to see whether a newer
  release exists (`update_check: false` turns this off; nothing about your setup is sent).
- **Anonymous usage statistics** to `pdb-print-stats.cloudline.no`, run by the maintainer, at most once a
  week. Nothing is sent until you have made a choice in the setup wizard or the web UI, where it can be
  switched off; [docs/telemetry.md](telemetry.md) lists every field and every way to turn it off.

Otherwise, this program will not transfer any information to other networked systems unless specifically
requested by the user or the person installing or operating it.

## Reporting a problem

If you think a signed pdb-print file is malicious or was not built from this repository, open an issue or
contact the maintainer through GitHub. Reports are investigated together with SignPath Foundation.
