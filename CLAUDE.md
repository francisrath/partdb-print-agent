# partdb-print-agent – context for Claude Code

A cross-platform, MIT-licensed label print agent for [Part-DB](https://github.com/Part-DB/Part-DB-server).
It fetches label PDFs that Part-DB already generates, rasterises them at the printer's native
resolution and sends them straight to label printers – no OS driver scaling.

The full design is in `docs/architecture.md` (read §0 TL;DR, §4 job flows, §5 data model,
§6 rendering, §7 drivers, §10 security, §19 MVP scope, §22 decisions). This file is the
working brief for the **MVP**.

## Decisions (2026-10-27)
- Language **Go**, single static binary, no cgo in the default build. Module: `github.com/francisrath/partdb-print-agent`.
- Licence **MIT**. Never link GPL/AGPL code (no brother_ql, no MuPDF, no Poppler). Brother raster
  protocol is implemented clean-room from Brother's public *Raster Command Reference* (QL-800/810W/820NWB).
- PDF rendering: **PDFium via go-pdfium in WebAssembly mode (wazero)** – BSD/Apache, no cgo.
- MVP printer scope: **Brother QL only** (TCP 9100 first, USB after) + a `virtual` printer that writes PNGs for tests.
  Everything else (ZPL, TSPL, ESC/POS, DYMO, Niimbot, Phomemo, OS queue, A4 sheets, two-colour) is phase 2,
  but keep the driver/transport interfaces general enough for them (see architecture §7).
- Part-DB token: **read-only** API token (enough for `POST /api/labels/generate`).
- No Part-DB changes needed for the MVP. Upstream issue is posted only **after** the v0.1.0 release.
- Tag inbox (print parts tagged `print-label`) was **dropped** (2026-10-27, closed PR #5): tagging a part just to
  print it was too much hassle. Printing from a phone/remote Part-DB waits for the upstream button + PrintJob API.

## MVP milestones (do them in order, each ends with something runnable)
1. **Skeleton** – `cmd/pdb-print` (cobra or plain flag subcommands), `internal/...` packages, config file
   (YAML, OS-appropriate path, `--config` override), structured logging, `pdb-print version`.
2. **Renderer** – PDF page → 1-bit raster at exact printhead dots.
   - Fit page to the printable width in dots (29 mm continuous = 306 dots, 12 mm = 106, 62 mm = 696; see Brother spec table).
   - Auto-rotate: if the page's long side is across the tape, rotate 90°. Support both the current portrait
     PDFs (29×46 mm with CSS-rotated content) and plain landscape PDFs (e.g. 50×29 mm).
   - Render without anti-aliasing / threshold (no dithering of barcodes) – QR codes must stay crisp.
   - Golden-image tests in `testdata/`.
3. **Brother QL driver + TCP transport** – raster commands (invalidate, initialise, status request, media
   info, auto-cut, cut-each, margins/feed, raster lines with optional compression, print with feed).
   Model table for at least QL-800/810W/820NWB; media table for continuous 12/29/62 mm and common die-cut sizes.
   Read status back over TCP where supported; map errors (no media, wrong media, cover open).
4. **CLI print paths**
   - `pdb-print print file.pdf --printer ql810` (local PDF – replaces `reference/qlprint`)
   - `pdb-print print --profile 2 --ids 32,112-125 --type part` (fetch via Part-DB API, then print)
   - `pdb-print printers test ql810` (test label)
5. **Local HTTP API + minimal web UI** on `127.0.0.1:8631` – instance + token setup, printer setup,
   profile→printer routes, test print, job history; `/v1/handoff` endpoint for a bookmarklet
   (top-level navigation, pairing key; Part-DB's CSP blocks fetch() to localhost – see architecture §1, §10).
6. **Hot folder** – watch a folder (default `~/Downloads`) for `label_*.pdf` and print them, then move/delete.
7. **Packaging** – goreleaser: darwin universal, linux amd64/arm64, windows x64; Dockerfile; Homebrew tap
   (`francisrath/tap`). Signing (Apple Developer ID via existing developer account, SignPath for Windows) comes later.

## Test environment (the author's setup)
Addresses below are placeholders; the real printer IP and Part-DB URL live in the gitignored
`config.local.yaml` and `.env`. Never commit them.
- Printer: **Brother QL-810W**, `tcp://192.168.1.50:9100`, roll **DK-22210 (29 mm continuous)** loaded.
  Also owns DK-22214 (12 mm) and DK-11204 (17×54 die-cut).
- Part-DB 2.20.x at **https://partdb.example.org** (behind Traefik; multipart POSTs get HTTP 418 there – irrelevant
  for the agent, which only uses JSON API calls).
- Label profiles in that instance (ids): 1 = part lid 12 mm (12×16 portrait, rotated content),
  2 = part bag 29 mm (29×46 portrait, rotated content), 3 = IC bag 17×54 die-cut, 4 = storage location 29×84.
- Known-good test: profile 2, part 32 (AMS1117-3.3).
- Reference implementation that already prints correctly: `reference/qlprint`
  (pdftoppm to exact dots → `brother_ql` over TCP). Use it to compare output; do not copy GPL code from brother_ql.

## Conventions
- Go ≥ 1.23, `go vet` + `staticcheck` clean, table-driven tests, no global state.
- Package layout suggestion: `internal/config`, `internal/partdb`, `internal/render`, `internal/driver/brotherql`,
  `internal/transport/{tcp,usb}`, `internal/queue`, `internal/server`, `internal/source/hotfolder`.
- Keep the `Driver` and `Transport` interfaces from architecture §7 so phase-2 drivers drop in.
- Never send real print jobs from tests; use the virtual printer. Hardware tests behind a build tag (`//go:build hardware`).
- Commit messages: conventional commits.
