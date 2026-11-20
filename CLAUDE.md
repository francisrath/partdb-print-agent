# partdb-print-agent – context for Claude Code

A cross-platform, MIT-licensed label print agent for [Part-DB](https://github.com/Part-DB/Part-DB-server).
It fetches label PDFs that Part-DB already generates, rasterises them at the printer's native
resolution and sends them straight to label printers – no OS driver scaling.

The design is in `docs/architecture.md` (§0 TL;DR, §4 job flows, §5 data model, §6 rendering, §7 drivers and
transports, §10 security, §22 decisions); per-phase designs and plans live in the author's vault
(`~/Notes/claude/partdb-agent-*`). Release steps: `docs/releasing.md`.

## Decisions
- Language **Go**, single static binary, `CGO_ENABLED=0`. The only cgo is the macOS Bluetooth helper
  `cmd/pdb-print-ble` (CoreBluetooth/IOBluetooth), built on a macOS release runner. Module:
  `github.com/francisrath/partdb-print-agent`.
- Licence **MIT**. Never link or copy GPL/AGPL code (no brother_ql, no MuPDF, no Poppler library, no GPL cat-printer
  projects). Protocols are implemented clean-room from public references (Brother Raster Command Reference, Zebra
  ZPL II guide, Epson ESC/POS, public cat-printer write-ups).
- PDF rendering: **PDFium via go-pdfium in WebAssembly mode (wazero)**; on 32-bit Linux it uses the external
  `pdftoppm` program (run as a process, not linked) when installed, because it is much faster there.
- **Pre-release: no backwards-compatibility code** (old config shapes, aliases, migration shims). Breaking config
  changes are fine; update `config.example.yaml` and the README instead.
- Part-DB token: **read-only** API token (enough for `POST /api/labels/generate`).
- Upstream: proposal [Part-DB-server#1625](https://github.com/Part-DB/Part-DB-server/issues/1625) and draft PRs
  (print button → local agent, server-side agent jobs, Mercure push, MCP) await the maintainer. Part-DB-server
  branches go to the `fork` remote only; nothing is pushed or posted upstream without the user's go-ahead.
- Tag inbox (print parts tagged `print-label`) was **dropped** (2026-10-27): too much hassle.

## What exists
- **Drivers** (`internal/driver/<family>`, registered by family, models are data in `internal/devicedb/data/*.yaml`):
  `brother` (QL raster: QL-800/810W/820NWB; P-touch CBP raster: PT-9500PC), `zpl` (Zebra desktop ZPL printers),
  `cat` (Bluetooth LE "cat printers" GB/GT/YT/MX), `escpos` (ESC/POS raster: REGO RG-MLP80B, generic 58/80 mm).
- **Transports** (`internal/transport/<kind>`, registered by config kind): `tcp`, `usb` (Linux), `ble` (Linux/Windows
  in-process, macOS via the helper), `bt` (Bluetooth Classic RFCOMM: Linux/Windows sockets, macOS via the helper).
- **Sources**: CLI `print`, local HTTP API + web UI and setup wizard on `127.0.0.1:8631` (`/v1/handoff` for the
  bookmarklet), hot folder (`internal/source/hotfolder`), server-side agent jobs (`internal/source/agentjobs`).
- **Other packages**: `config`, `partdb`, `render`, `printing` (the print service: per-printer lock, media, status,
  online), `discovery`, `try` (guided first print), `report`, `snmp`, `pdfgen` (test labels), `server`,
  `blecore`/`bleradio`/`blebridge` (shared Bluetooth code and the helper's stdin/stdout protocol).
- Packaging: GoReleaser (Linux/Windows/.deb/Docker on Ubuntu; macOS + Homebrew cask on a macOS runner).

## Test environment (the author's setup)
Addresses below are placeholders; real addresses, the Part-DB URL and tokens live in the gitignored
`config.local.yaml` and `.env`. Never commit them.
- Brother **QL-810W**, `tcp://192.168.1.50:9100`, roll **DK-22210 (29 mm continuous)**; also DK-22214 (12 mm) and
  DK-11204 (17×54 die-cut). Brother **PT-9500PC** on a Raspberry Pi agent over USB. **MX10** cat printer (BLE).
  A REGO **RG-MLP80B** (Bluetooth Classic, ESC/POS) arrives week of 2026-11-23. Raspberry Pis and a Windows 11 laptop
  serve as test agents.
- Part-DB 2.20.x at **https://partdb.example.org** (behind Traefik; multipart POSTs get HTTP 418 there – irrelevant
  for the agent, which only uses JSON API calls).
- Label profiles in that instance (ids): 1 = part lid 12 mm (12×16 portrait, rotated content),
  2 = part bag 29 mm (29×46 portrait, rotated content), 3 = IC bag 17×54 die-cut, 4 = storage location 29×84.
- Known-good test: profile 2, part 32 (AMS1117-3.3).
- `reference/qlprint` (pdftoppm → `brother_ql` over TCP) printed correctly before the agent existed; use it only to
  compare output, never copy its GPL code.

## Conventions
- Go version from `go.mod`; `go vet` + `staticcheck` clean on Linux, Windows and macOS; table-driven tests; no global
  state beyond the driver/transport registries.
- Keep the `Driver` and `Transport` interfaces (architecture §7) so new families and kinds drop in; a new model of an
  existing family is a device-database entry, not code.
- Tests never send real print jobs or touch real Bluetooth/USB: fake transports, pipes, fake helper processes.
  Hardware tests sit behind the `hardware` build tag (`//go:build hardware`).
- Commit messages: conventional commits, no AI attribution lines. PR descriptions end with the AI-use line.
