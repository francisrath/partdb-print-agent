# Contributing

Bug reports, printer reports and pull requests are welcome. Building, testing and the code layout are in the
README ([Development](README.md#development), [Adding a printer](README.md#adding-a-printer)); the design is in
[`docs/architecture.md`](docs/architecture.md).

## Issues

- **Bug:** use the bug report form. Attach the output of `pdb-print report` if a printer is involved, and remove
  your Part-DB address and token from any config you paste.
- **A printer that isn't supported yet:** use the printer support form and attach `pdb-print report --identify`
  (it asks before querying each printer, since a printer that doesn't understand a query may print it as text). What else helps
  is in [docs/printers.md](docs/printers.md#supporting-a-new-printer). If we have no such printer, the issue
  becomes a "looking for a tester" issue (label `help wanted`).
- **An untested printer (🟡 in [docs/printers.md](docs/printers.md)):** see
  [Test an untested printer](docs/printers.md#test-an-untested-printer).
- **Part-DB itself** (label profiles, the API, the print button): [Part-DB-server](https://github.com/Part-DB/Part-DB-server/issues).
- **Security issues:** not in a public issue, see [SECURITY.md](SECURITY.md).

## Pull requests

- `go vet ./...`, `staticcheck ./...` and `go test ./...` must pass (CI runs vet and tests on Linux, Windows and macOS, staticcheck on Linux).
  The binary builds with `CGO_ENABLED=0` (only the macOS Bluetooth helper `cmd/pdb-print-ble` uses cgo).
- Tests never send real print jobs or touch real USB or Bluetooth: use fake transports, pipes or fake helper
  processes. Tests that need hardware go behind the `hardware` build tag.
- A new model of a driver family pdb-print already has is a device-database entry in
  `internal/devicedb/data/*.yaml`, not code. Run `go test ./internal/devicedb -run TestPrintersDoc -update`
  afterwards to regenerate the tables in `docs/printers.md`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat(zpl): …`,
  `fix: …`, `docs: …`).
- pdb-print is pre-release: change config shapes freely, without compatibility shims; update
  `config.example.yaml` and the README instead.

## Licence and clean-room rule

pdb-print is MIT. Contributions are accepted under the same licence. Never copy or link GPL/AGPL code
(e.g. brother_ql, MuPDF, the Poppler library, GPL cat-printer projects), not even "just the protocol part":
implement protocols from public documentation (manufacturer command references, public write-ups) and say in the
PR which references you used.
