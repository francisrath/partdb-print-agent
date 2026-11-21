# Usage statistics

pdb-print can send an anonymous usage report once a week. It helps decide which printers and platforms to
work on, and shows how many installs are out there. This page lists everything the report contains.

## When it is sent

- **Only after you chose.** The last step of the setup wizard, and the "Usage statistics" section of the
  web UI, have a box for it (ticked by default). Until you've passed that step or saved that section,
  nothing is sent. Without a choice in the config (`telemetry:` missing) nothing is sent either.
- **At most once a week**, for the previous week's numbers: the first report goes out in the week after
  the agent first counted something. Weeks counted while the report was off are thrown away, not sent later.
- **Only by `pdb-print serve`**, and only by released versions; development builds never send.
- If sending fails, it tries again hourly that week. It never holds up printing.

## Turning it off

Any of these:

- untick the box in the web UI ("Usage statistics") or at the end of the setup wizard;
- `telemetry: false` in the config;
- the environment variable `DO_NOT_TRACK=1` or `PDB_PRINT_TELEMETRY=off`.

`pdb-print telemetry show` prints the next report exactly as it would be sent, and whether it will be.

## What is sent

One JSON document to `https://pdb-print-stats.cloudline.no/v1/report`:

| Field | Example | What it is |
|---|---|---|
| `schema` | `1` | report format version |
| `week` | `"2026-W47"` | the ISO week the numbers cover |
| `version` | `"0.3.0"` | pdb-print version |
| `os` | `"linux"` | operating system |
| `arch` | `"arm"` | CPU architecture |
| `os_version` | `"debian 13"` | distribution and major version, `"macos 15"`, `"windows 11 build 26100"` |
| `board` | `"Raspberry Pi 2 Model B"` | single-board computer model (Linux device tree), else empty |
| `install` | `"deb"` | how pdb-print was installed: `deb`, `brew`, `docker`, `zip` or `archive` |
| `renderer` | `"pdftoppm"` | PDF renderer in use: `pdfium` or `pdftoppm` |
| `agent_api` | `true` | the connected Part-DB has the print agent API (print jobs from Part-DB) |
| `models` | `{"QL-810W": 1}` | configured printers per model, by device database id |
| `models_unknown` | `0` | configured printers whose model isn't in the device database (only counted) |
| `transports` | `{"tcp": 1, "usb": 1}` | configured printers per connection type |
| `jobs` | `{"bookmarklet": 12}` | print jobs that week per source: `bookmarklet`, `part-db`, `hot-folder`, `web-ui`, `setup` |
| `labels` | `{"QL-810W": 40}` | labels printed that week per model |
| `errors` | `{"NO_MEDIA": 1}` | failed jobs that week per error code |

## What is never sent

IP or MAC addresses, host names, Bluetooth addresses or names, USB serial numbers, printer names or ids
from your config, the Part-DB URL or token, label content or part data, file names, and timestamps finer
than the week. There is no install id: reports can't be tied to each other or to an install. The
collector only accepts word-like values (letters, digits, `. _ + -`), so free text can't slip in.

## Where it goes

The collector is `cmd/pdb-print-stats` in this repository. It checks each report against the list above,
appends it with the day it arrived to a file, and keeps nothing else: it doesn't read or store the
sender's address, the web server in front of it doesn't log the address for this site, and no cookies
are set. Reports are kept for the statistics; aggregate numbers may be published.
