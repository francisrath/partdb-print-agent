<p align="center"><img src="assets/icon/social-preview.png" alt="pdb-print – Labelbot, direct label printing for Part-DB" width="100%"></p>

# partdb-print-agent

Direct label printing for [Part-DB](https://github.com/Part-DB/Part-DB-server).

A small cross-platform agent (`pdb-print`) that takes the label PDFs Part-DB already generates,
rasterises them at the printer's native resolution and sends them straight to your label printer –
crisp QR codes, exact sizes, automatic cutting, no fiddling with OS print dialogs.

**Status:** early development. First target: Brother QL printers over the network.

- Works with Part-DB as it is today (read-only API token).
- Planned: Zebra/ZPL, TSPL, ESC/POS, DYMO, Niimbot, Phomemo, normal printers and A4 label sheets;
  instant printing from any device through NAT via an optional Part-DB extension, proposed upstream in
  [Part-DB-server#1625](https://github.com/Part-DB/Part-DB-server/issues/1625).

## Install

**macOS / Linux (Homebrew):**
```sh
brew tap francisrath/tap
brew trust francisrath/tap      # Homebrew 7 only loads casks from trusted taps
brew install --cask pdb-print
```
The macOS binary isn't signed yet; the cask clears the quarantine flag on install.

**Docker** (Linux amd64/arm64):
```sh
docker run --rm --network host --stop-timeout 130 \
  -v pdb-print:/home/nonroot/.config/pdb-print \
  ghcr.io/francisrath/partdb-print-agent:latest
```
On stop, the agent lets a print in progress finish (up to 2 minutes), hence `--stop-timeout 130`.
The web UI binds loopback only, so `--network host` makes it reachable at `http://127.0.0.1:8631`
on the host. In a container there is no OS keychain: the token is kept in the config file (0600).

**Anything else:** archives for macOS (universal), Linux and Windows are on the
[releases page](https://github.com/francisrath/partdb-print-agent/releases).

## Usage (early)

```sh
go build -o bin/pdb-print ./cmd/pdb-print
cp config.example.yaml config.local.yaml   # set your printer's address
cp .env.example .env          # fill in PDB_PRINT_URL and PDB_PRINT_TOKEN (Read-Only API token), then:
set -a; . ./.env; set +a      # loads PDB_PRINT_URL / PDB_PRINT_TOKEN / PDB_PRINT_CONFIG

bin/pdb-print printers status ql810                           # model, errors, loaded roll
bin/pdb-print printers test ql810                             # test label sized for the media
bin/pdb-print print --profile 2 --ids 32 --printer ql810      # fetch from Part-DB and print
bin/pdb-print print --profile 2 --ids 32,112-125 --type part --printer ql810
bin/pdb-print print label.pdf --printer ql810                 # local PDF
bin/pdb-print render label.pdf --printer ql810 -o /tmp        # PNG preview at head dots
bin/pdb-print media                                           # known Brother QL media
```
### Web UI and bookmarklet

```sh
bin/pdb-print serve            # http://127.0.0.1:8631
```
In the UI: connect Part-DB (the token goes to the OS keychain), add printers, map label profiles to
printers, then drag the generated bookmarklet to your bookmarks bar. On a part or storage-location page
in Part-DB, clicking it opens a small window that prints the label. The bookmarklet signs each request
with a per-install key, so other websites can't print through the agent without a click.

A proposed Part-DB change adds a Print button and printer icons to Part-DB itself (draft, see
[#1625](https://github.com/Part-DB/Part-DB-server/issues/1625)). With such a Part-DB, click
"Connect Part-DB's print button to this agent" in the UI once; the button then works like the bookmarklet.

Before each job the agent reads the printer over SNMP (UDP 161): the loaded roll, errors
(cover open, wrong media, empty) and, after sending, whether the label actually printed. A job is
refused with `MEDIA_MISMATCH` if the roll differs from the configured `media`, or if the Part-DB
label size doesn't fit the roll (`allow_scaling: true` prints it scaled instead).

### Hot folder

With `hot_folder` enabled in the config, `serve` prints every new `label_*.pdf` that lands in
`~/Downloads` (Part-DB's label download name) on the configured printer, then moves it to
`printed/` (or `failed/`). Files older than 60 s, e.g. ones already there at startup, are never printed.

Add `--dry-run job.bin` to any print command to write the printer byte stream instead.
Printers (and optionally Part-DB instances with a `token:`) live in a YAML config:
see `config.example.yaml`; copy it to `config.local.yaml` (gitignored) to customise.

Tests: `go test ./...` (never prints; a fake printer runs on localhost).
`go test ./... -update` rewrites golden files – inspect the diff first.
Icons: edit the SVGs in `assets/icon/` (or `docs/logo.svg`) and run `scripts/gen-icons.sh`
(needs `brew install librsvg imagemagick`, plus `iconutil` from macOS); the output is committed.

Status-only hardware check: `PDB_PRINT_QL_ADDR=host:9100 go test -tags hardware ./internal/driver/brotherql`.

See [`docs/architecture.md`](docs/architecture.md) for the full design.

## License
MIT
