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

**Raspberry Pi OS, Debian, Ubuntu** (a print server, e.g. a Raspberry Pi next to the printer):

> **Before installing:** for up to 30 minutes the setup page is open to the network (see below), protected
> only by a 6-digit code. Check who can reach TCP port 8631 on the print server: anyone who can, and
> sees the code first, can set it up. On a network with devices you don't trust, or a print server
> connected straight to your internet provider, limit port 8631 to trusted machines with a firewall
> first. pdb-print only filters out obviously remote addresses; that is not a firewall.

```sh
curl -fsSL https://github.com/francisrath/partdb-print-agent/releases/latest/download/install.sh | sh
```
Or read it first: download `install.sh` from the release, read it (about 40 lines), then `sh install.sh`.
The script picks the `.deb` package for your system, checks it against the release's `checksums.txt`,
installs it with `apt-get` (the one step that needs `sudo`) and shows a setup code. The package adds:
- `/usr/bin/pdb-print`;
- a `pdb-print` system user, allowed to use USB printers (group `lp`);
- a `pdb-print` systemd service; its config is `/var/lib/pdb-print/config.yaml`.

Its scripts only create that user and an empty config, enable and (re)start the service, and stop
it on removal, through Debian's `deb-systemd-helper`/`deb-systemd-invoke`, so a service you disabled
stays disabled on upgrades (`packaging/deb/`).
On 32-bit Raspberry Pi OS it also installs `poppler-utils`, whose `pdftoppm` renders labels much faster there.

Then, on any computer on the same network, open the address the script shows
(`http://<pi-name>.local:8631/setup/`) and type the 6-digit code. A wizard connects Part-DB, finds
your printer, prints a test label and names it. The setup page is reachable from the network only
for 15 minutes after the code was shown (longer while you use it, up to 30); after that the agent
answers only on the machine itself again. Run `pdb-print pair` on the print server to open it again.

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
on the host (and lets it find network printers). Until Part-DB is connected, the setup wizard is
open to the network for a while: the code is in `docker logs`, and
`docker exec <container> pdb-print pair` opens it again. In a container there is no OS keychain: the
token is kept in the config file (0600).

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
bin/pdb-print media                                           # known media and the models that take them
bin/pdb-print discover                                        # find printers on USB and the network
```

`discover` only connects to the print port and asks SNMP; it never sends anything a printer could
print. For a printer pdb-print doesn't support yet, `pdb-print report` writes a file to attach to a
GitHub issue: discovery data, SNMP values and, with `--identify` (asks per printer, since a printer
that doesn't understand a query may print it), the replies to each printer language's identify
query; `--print` adds a test label on supported printers. IP/MAC addresses, serial numbers (USB and
IEEE 1284 device ID; only the last 4 characters of long serials stay, so the same printer can be
recognised across reports; inside printer replies only from 3 characters on) and the host name are
masked unless `--raw` – check the file before you post it.

Each printer in the config names its `model` (from the device database; it picks the driver). USB
printers on Linux are found by `usb_id` (and `serial`, for two identical printers) at every job, so
replugging doesn't matter; `device: /dev/usb/lpN` works too. `mirror: true` and `upside_down: true`
flip the printed image; the setup wizard sets them when the test label comes out mirrored or upside down. The agent reports each printer's online
state to Part-DB with every heartbeat.
### Web UI and bookmarklet

```sh
bin/pdb-print serve            # http://127.0.0.1:8631
```
In the UI: connect Part-DB (the token goes to the OS keychain), add printers ("Add a printer" runs
the setup wizard: find, test print, name), map label profiles to printers, then drag the generated bookmarklet to your bookmarks bar. On a part or storage-location page
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

### Zebra (ZPL)

`model:` a Zebra from the device database (GK420d/t, GX420d/t, GX430t, GC420d/t, ZP450, ZP500, LP2844-Z,
ZD220/230, ZD410, ZD420/421/620/621 at 203 or 300 dpi; `pdb-print media` lists them). Over the network
(port 9100) or USB on Linux. Zebras don't report their label *width*, so set `media:` to the loaded size
(`label-102x102` for 4×4", `label-102x152` for 4×6" …); the printer's reported label length is checked
against it. Every label goes as one compressed graphic; the agent reads the printer's status and waits for
the print buffer to empty to confirm a label. It never saves settings or calibrates the printer.
Zebras that don't answer SNMP (older G-series) show up as "unknown" in `discover`; `pdb-print try` and the
setup wizard can ask them what they are (a few bytes a non-Zebra might print).

EPL-only printers (plain LP/TLP 2844 without "-Z") and serial (RS-232) connections aren't supported yet.

### Bluetooth label printers ("cat printers")

`model:` GB01, GB02, GB03, GT01, YT01, MX05, MX06, MX08 or MX10: small Bluetooth LE thermal printers
with 57 mm paper (384 dots at 203 dpi), continuous (sticker rolls without gaps). A printer is matched by
its advertised Bluetooth name, which must be exactly the model name.

```yaml
- id: cat
  name: Bench MX10
  model: MX10
  transport: {kind: ble, name: MX10, address: "AA:BB:CC:DD:EE:FF"}
  media: paper-57
  darkness: normal            # light | normal | dark; darker prints slower
```

`darkness` sets the print speed (slower is darker). On an MX10, `dark` looks about the same as `normal`;
`light` is visibly lighter and faster.

`pdb-print discover` lists nearby Bluetooth printers with their address and the exact `transport:` line
(the Bluetooth scan can make `discover` take up to about 8 s; `--no-ble` skips it), and the setup wizard lists them too. The address
is the platform's device id: a MAC address on Linux and Windows, a CoreBluetooth UUID on macOS. With only
`name`, the agent listens about 2 s longer after the first match and refuses to guess if two printers
share the name (the error ends "set the address instead"); with `address`, the address wins. `pdb-print try` supports cat printers
(status, test card, part label, and a "too pale?" question that reprints darker).

A printer holds one connection at a time: disconnect it from phones and other apps first.

- **Linux:** through BlueZ over D-Bus. The `.deb` service runs with the groups `lp` and `bluetooth` (the
  package creates `bluetooth` if missing). The adapter must be on: `rfkill unblock bluetooth`,
  `bluetoothctl power on`. If BlueZ still holds the printer, `bluetoothctl disconnect <address>`.
- **Windows:** in-process, needs a Bluetooth 4.0+ (LE) adapter; older Bluetooth 3.0 radios (e.g. Dell
  Wireless 375) can't.
- **macOS:** Bluetooth goes through the helper `pdb-print-ble`, shipped next to `pdb-print` in the macOS
  archive and the Homebrew cask (if it's missing, the error says to install it). Only the helper asks for
  Bluetooth permission, once, the first time a Bluetooth printer is used; the prompt names the app that ran
  it (e.g. Terminal). Other commands never trigger it. The helper isn't Developer-ID signed yet, so macOS
  may ask again after an update.
- **Docker:** no Bluetooth; the container can't reach the host's D-Bus/BlueZ.

### Testing a printer: `pdb-print try`

`pdb-print try [HOST]` is a guided test for someone standing at the printer (Zebra for now): it finds the
printer, asks before sending anything, prints numbered test labels (test card, a part label with QR code and
barcode, a batch of two, a rotated label), asks what came out, tries the fix for a "no" right away (moved,
widened, turned, mirrored), checks the status with the lid open, puts the printer's settings back and writes
`pdb-print-try-<model>-<time>.txt` (addresses and serial numbers masked). The Windows zip has
`try-printer.cmd` and `TRY.md` for a double-click run.

### Brother P-touch PT-9500PC (USB, Linux)

`model: PT-9500PC` drives the tape printer over USB through Linux's `usblp` driver (add the user to
group `lp`). Brother QLs also work over USB the same way (QL-800, QL-810W, QL-820NWB). It reads the loaded tape and errors before each job,
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
install poppler (`sudo apt install poppler-utils`): pdb-print then renders with `pdftoppm` in about
a second. On 32-bit systems it's used automatically whenever `pdftoppm` and `pdfinfo` (both in
poppler-utils) are on `PATH`; `pdftoppm: off` in the config (or `PDB_PRINT_PDFTOPPM=off`) turns that
off, `on` uses it on any system. Note that poppler parses PDFs natively, while the built-in renderer
runs in a WebAssembly sandbox.

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

See [`docs/architecture.md`](docs/architecture.md) for the full design and [`docs/releasing.md`](docs/releasing.md) for how releases are made.

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
