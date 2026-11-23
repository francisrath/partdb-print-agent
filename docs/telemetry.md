# Usage statistics

pdb-print can send an anonymous usage report once a week. It helps decide which printers and platforms to
work on, and shows how many installs are out there. This page lists everything the report contains.

## When it is sent

- **Only after you chose.** The last step of the setup wizard, and the "Usage statistics" section of the
  web UI, have a box for it (ticked by default). Until you've passed that step or saved that section,
  nothing is sent. Without a choice in the config (`telemetry:` missing) nothing is sent either.
- **At most once a week**, for the previous week's numbers (jobs in the first hour of a new week can land
  in the previous one): the first report goes out in the week after the agent first counted something.
- **Nothing is counted while the report is off or undecided**, and switching it on starts from zero, so
  nothing from before your choice is ever sent.
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
| `id` | `"3f9c0a1b2d4e5f60"` | random, new every week: a report sent twice (a lost reply) counts once; never reused |
| `week` | `"2026-W47"` | the ISO week the numbers cover |
| `version` | `"0.3.0"` | pdb-print version |
| `os` | `"linux"` | operating system |
| `arch` | `"arm"` | CPU architecture |
| `os_version` | `"debian 13"` | distribution and major version from a fixed list (others are just `"linux"`), `"macos 15"`, `"windows 11 build 26100"`; in Docker, the container's |
| `board` | `"Raspberry Pi 2 Model B"` | Raspberry Pi model (Linux device tree), else empty |
| `install` | `"deb"` | how pdb-print was installed: `deb`, `brew`, `docker`, `windows` (setup.exe or `pdb-print install`), `zip` or `archive` |
| `renderer` | `"pdftoppm"` | PDF renderer in use: `pdfium` or `pdftoppm` |
| `agent_api` | `true` | the connected Part-DB has the print agent API (print jobs from Part-DB) |
| `models` | `{"QL-810W": 1}` | configured printers per model, by device database id |
| `models_unknown` | `0` | configured printers whose model isn't in the device database (only counted) |
| `transports` | `{"tcp": 1, "usb": 1}` | configured printers per connection type |
| `jobs` | `{"bookmarklet": 12}` | print jobs that week per source: `bookmarklet`, `part-db`, `hot-folder`, `web-ui`, `setup` (wizard test prints), `other` |
| `labels` | `{"QL-810W": 40}` | labels printed that week per model |
| `errors` | `{"NO_MEDIA": 1}` | failed jobs that week per error code (the printer status codes, or `OTHER`) |

## What is never sent

IP or MAC addresses, host names, Bluetooth addresses or names, USB serial numbers, printer names or ids
from your config, the Part-DB URL or token, label content or part data, file names, and timestamps finer
than the week. There is no install id. Every value comes from a fixed list (models from the device
database, the connection types, job sources and error codes above, known operating systems), and the
collector rejects anything else, so free text can't slip in. Be aware that with few installs, an unusual
combination (a rare printer model on a rare board) can still make one install's weekly reports
recognisable as probably the same one.

## Where it goes

The collector is `cmd/pdb-print-stats` in this repository. It checks each report against the list above,
appends it with the day it arrived to a file, and keeps nothing else: it doesn't read or store the
sender's address, the web server in front of it doesn't log the address for this site, and no cookies
are set. It accepts reports only for the last eight weeks, stores at most 5,000 a day, and counts a
resent report (same `id`) once. Reports are kept for the statistics. The totals are public at
<https://pdb-print-stats.cloudline.no/>: installs per week and, for the latest week, versions, platforms,
printer models and connections. A badge with the latest week's installs:

```markdown
![active installs](https://img.shields.io/endpoint?url=https://pdb-print-stats.cloudline.no/v1/badge/installs.json)
```
