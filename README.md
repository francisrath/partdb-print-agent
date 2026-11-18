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
bin/pdb-print media                                           # known Brother QL media and PT tapes
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

**Experimental:** with a Part-DB that has the proposed print job API (not in upstream Part-DB yet; the API may
still change), `serve` also fetches print jobs from Part-DB: it reports
its printers every 30 s, and Part-DB's Print buttons (from any device, also a phone) send jobs to them. Nothing
to set up beyond the API token; the agent identifies itself with a random `agent_id` saved in the config.
If Part-DB has a Mercure hub set up, jobs arrive within a second; otherwise they wait for the next 30 s poll.
With an older Part-DB this stays off.

Before each job the agent reads the printer over SNMP (UDP 161): the loaded roll, errors
(cover open, wrong media, empty) and, after sending, whether the label actually printed. A job is
refused with `MEDIA_MISMATCH` if the roll differs from the configured `media`, or if the Part-DB
label size doesn't fit the roll (`allow_scaling: true` prints it scaled instead).

### Brother P-touch PT-9500PC (USB, Linux)

`driver: brother-pt` drives a PT-9500PC tape printer over USB through Linux's `usblp` driver
(`/dev/usb/lp0`; add the user to group `lp`). It reads the loaded tape and errors before each job,
sends a batch label by label and waits for each to be printed. Labels print at true size: design the
Part-DB profile at the full tape width and keep the unprintable edges clear (4.5 mm on 36 mm tape,
about 1 mm on narrower tapes). Part-DB places label content over the whole page, so for 36 mm tape
inset it in the profile's additional CSS, e.g. for a 50×36 mm profile:
`@page { margin: 0; } .page-inner { top: 5mm; left: 2mm; width: 46mm; height: 26mm; }`. The cutter sits ~25 mm past the print head, so by default the last label
waits in the printer and comes out, cut, with the next print (or press the cut button); a batch on
laminated tape comes out as one half-cut strip. `cut: per_label` feeds and cuts at once instead, at the
cost of a blank leader per job. See `config.example.yaml`.

On a Raspberry Pi: use the `linux_armv6` release on 32-bit Raspberry Pi OS (Pi 1, Zero, Pi 2 v1.1) or
`linux_arm64` on 64-bit, install it as `/usr/local/bin/pdb-print`, and run `pdb-print serve` from a systemd
unit with the API token in an `EnvironmentFile` (`PDB_PRINT_TOKEN=…`), since a headless Pi has no keychain.
The web UI stays on `127.0.0.1:8631`: reach it with `ssh -L 8631:127.0.0.1:8631 pi-host`.
On 32-bit ARM the built-in PDF renderer is slow (up to a minute or two per label on a Pi 1), so
install poppler (`sudo apt install poppler-utils`): pdb-print then renders with `pdftoppm` in
about a second. It's used automatically whenever it's on `PATH`; `pdftoppm: off` in the config
(or `PDB_PRINT_PDFTOPPM=off`) turns that off. Note that poppler parses PDFs natively, while the
built-in renderer runs in a WebAssembly sandbox.

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

Status-only hardware checks: `PDB_PRINT_QL_ADDR=host:9100 go test -tags hardware -count=1 ./internal/driver/brother`,
`PDB_PRINT_PT_DEVICE=/dev/usb/lp0 go test -tags hardware -count=1 ./internal/driver/brother`.

See [`docs/architecture.md`](docs/architecture.md) for the full design.

## Adding a printer

A printer that an existing driver family can already talk to (e.g. another Brother QL or PT with the
same protocol) is an entry in `internal/devicedb/data/*.yaml`: its id, driver and dialect, dpi and head
pins, how it identifies itself (USB id, IEEE 1284 model, status model code), which media it takes and
any quirks. Add a golden test vector for it if you can; `go test ./internal/devicedb` checks the data.
A new protocol family is a package under `internal/driver/` that registers itself in `init`; a new
connection type is a package under `internal/transport/`. Registration only runs when the binary imports
the package, so add a blank import next to the existing ones in `internal/printing/printing.go`.

Every printer in the config needs a `model` from the device database (`pdb-print media` lists what
the models take); a config with an unknown or missing model doesn't load.

## License
MIT
