<p align="center"><img src="assets/icon/social-preview.png" alt="pdb-print – Labelbot, direct label printing for Part-DB" width="100%"></p>

# partdb-print-agent

Direct label printing for [Part-DB](https://github.com/Part-DB/Part-DB-server).

A small cross-platform agent (`pdb-print`) that takes the label PDFs Part-DB already generates,
rasterises them at the printer's native resolution and sends them straight to your label printer –
crisp QR codes, exact sizes, automatic cutting, no fiddling with OS print dialogs.

**Status:** early development. First target: Brother QL printers over the network.

- Works with Part-DB as it is today (read-only API token).
- Planned: Zebra/ZPL, TSPL, ESC/POS, DYMO, Niimbot, Phomemo, normal printers and A4 label sheets;
  instant printing from any device through NAT via an optional Part-DB extension.

## Usage (early)

```sh
go build -o bin/pdb-print ./cmd/pdb-print
pdb-print --config config.example.yaml printers status ql810    # model, errors, loaded roll
pdb-print --config config.example.yaml render label.pdf --printer ql810 -o /tmp   # PNG preview at head dots
pdb-print --config config.example.yaml print label.pdf --printer ql810
pdb-print --config config.example.yaml print label.pdf --printer ql810 --dry-run job.bin
pdb-print media                                                  # known Brother QL media
```

Tests: `go test ./...` (never prints; a fake printer runs on localhost).
`go test ./... -update` rewrites golden files – inspect the diff first.
Icons: edit the SVGs in `assets/icon/` (or `docs/logo.svg`) and run `scripts/gen-icons.sh`
(needs `brew install librsvg imagemagick`, plus `iconutil` from macOS); the output is committed.

Status-only hardware check: `PDB_PRINT_QL_ADDR=host:9100 go test -tags hardware ./internal/driver/brotherql`.

See [`docs/architecture.md`](docs/architecture.md) for the full design.

## License
MIT
