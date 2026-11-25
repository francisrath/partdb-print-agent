# Printers

Which label printers pdb-print can drive, how well each one is tested, and how to help with the rest.
Every printer listed as ✅ or 🟡 is supported by the [latest release](https://github.com/francisrath/pdb-print/releases/latest).

## Support matrix

- ✅ tested OK: printed with on real hardware, by the **maintainer** or the **community**
- 🟡 implemented, not tested: written from the manufacturer's documentation; nobody has printed with it yet
- ⏳ not supported yet: see [below](#not-supported-yet)
- ❌ can't be supported: see [below](#cant-be-supported)

The tables below are generated from the device database (`internal/devicedb/data/*.yaml`). Don't edit
them by hand: change the data and run `go test ./internal/devicedb -run TestPrintersDoc -update`.

<!-- printers:begin -->

### Brother

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| ✅ maintainer | PT-9500PC | USB | brother/pt-cbp | USB on a Raspberry Pi and on Windows 11, 2026-11 |
| 🟡 | PT-E500 | USB | brother/pt-raster |  |
| 🟡 | PT-E550W | Network, USB | brother/pt-raster |  |
| 🟡 | PT-P700 | USB | brother/pt-raster |  |
| 🟡 | PT-P710BT | USB, Bluetooth Classic | brother/pt-raster |  |
| 🟡 | PT-P750W | Network, USB | brother/pt-raster |  |
| 🟡 | PT-P900 | USB | brother/pt-raster |  |
| 🟡 | PT-P900W | Network, USB | brother/pt-raster |  |
| 🟡 | PT-P910BT | USB, Bluetooth Classic | brother/pt-raster |  |
| 🟡 | PT-P950NW | Network, USB | brother/pt-raster |  |
| 🟡 | QL-1050 | USB | brother/ql |  |
| 🟡 | QL-1060N | Network, USB | brother/ql |  |
| 🟡 | QL-1100 | USB | brother/ql |  |
| 🟡 | QL-1110NWB | Network, USB | brother/ql |  |
| 🟡 | QL-1115NWB | Network, USB | brother/ql |  |
| 🟡 | QL-560 | USB | brother/ql |  |
| 🟡 | QL-570 | USB | brother/ql |  |
| 🟡 | QL-580N | Network, USB | brother/ql |  |
| 🟡 | QL-700 | USB | brother/ql |  |
| 🟡 | QL-710W | Network, USB | brother/ql |  |
| 🟡 | QL-720NW | Network, USB | brother/ql |  |
| 🟡 | QL-800 | USB | brother/ql |  |
| ✅ maintainer | QL-810W | Network, USB | brother/ql | TCP and SNMP status, 2026-10; USB on Linux and Windows 11, 2026-11 |
| 🟡 | QL-820NWB | Network, USB | brother/ql |  |

### DYMO

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | LabelWriter-330 | USB, Serial | dymo/lw300 |  |
| 🟡 | LabelWriter-330-Turbo | USB, Serial | dymo/lw300 |  |
| 🟡 | LabelWriter-400 | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-400-Turbo | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-450 | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-450-Duo | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-450-Turbo | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-450-Twin-Turbo | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-4XL | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-Duo | USB | dymo/lw400 |  |
| 🟡 | LabelWriter-Twin-Turbo | USB | dymo/lw400 |  |

### Generic

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | GB01 | Bluetooth LE | cat/gb |  |
| 🟡 | GB02 | Bluetooth LE | cat/gb |  |
| 🟡 | GB03 | Bluetooth LE | cat/gb |  |
| 🟡 | GT01 | Bluetooth LE | cat/gb |  |
| 🟡 | MX05 | Bluetooth LE | cat/gb |  |
| 🟡 | MX06 | Bluetooth LE | cat/gb |  |
| 🟡 | MX08 | Bluetooth LE | cat/gb |  |
| ✅ maintainer | MX10 | Bluetooth LE | cat/gb | Bluetooth LE on Linux, Windows and macOS, 2026-11 |
| 🟡 | YT01 | Bluetooth LE | cat/gb |  |
| 🟡 | ESCPOS-58 | Bluetooth Classic | escpos/raster |  |
| 🟡 | ESCPOS-80 | Bluetooth Classic | escpos/raster |  |

### HPRT

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | HT300 | USB, Network, Bluetooth Classic | tspl |  |
| 🟡 | N41 | USB | tspl |  |

### iDPRT

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | SP410 | USB | tspl |  |
| 🟡 | SP420 | USB | tspl |  |
| 🟡 | iD4S-203 | USB, Network | tspl |  |
| 🟡 | iD4S-300 | USB, Network | tspl |  |

### Munbyn

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | ITPP941 | USB | tspl |  |

### Niimbot

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | B1 | Bluetooth LE | niimbot/b1 |  |
| ✅ maintainer | B21S | Bluetooth LE, Serial | niimbot/b1 | Bluetooth LE on a Raspberry Pi and USB serial on macOS, 2026-11 (firmware 40.27 and 40.36) |
| 🟡 | D101 | Bluetooth LE | niimbot/b1 |  |
| 🟡 | B21 | Bluetooth LE | niimbot/b21 |  |
| 🟡 | D11 | Bluetooth LE | niimbot/d11 |  |
| 🟡 | D11S | Bluetooth LE | niimbot/d11 |  |
| 🟡 | B18 | Bluetooth LE | niimbot/d110 |  |
| 🟡 | D110 | Bluetooth LE | niimbot/d110 |  |
| 🟡 | B1-PRO | Bluetooth LE | niimbot/v4 |  |
| 🟡 | B21-PRO | Bluetooth LE | niimbot/v4 |  |
| 🟡 | D11-H | Bluetooth LE | niimbot/v4 |  |

### Phomemo

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | D30 | Bluetooth LE | phomemo/d30 |  |
| 🟡 | D35 | Bluetooth LE | phomemo/d30 |  |
| 🟡 | M02 | Bluetooth LE | phomemo/m02 |  |
| 🟡 | M02-PRO | Bluetooth LE | phomemo/m02 |  |
| 🟡 | M02S | Bluetooth LE | phomemo/m02 |  |
| 🟡 | T02 | Bluetooth LE | phomemo/m02 |  |
| 🟡 | M110 | Bluetooth LE | phomemo/m110 |  |
| 🟡 | M120 | Bluetooth LE | phomemo/m110 |  |
| 🟡 | M200 | Bluetooth LE | phomemo/m110 |  |
| 🟡 | M220 | Bluetooth LE | phomemo/m110 |  |
| 🟡 | M221 | Bluetooth LE | phomemo/m110 |  |
| 🟡 | Q30 | Bluetooth LE | phomemo/m110 |  |

### REGO

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| ✅ maintainer | RG-MTP80B | Bluetooth Classic | escpos/raster | Bluetooth Classic on macOS, 2026-11. Some shops list it as RG-MLP80B. Darkness is the printer's own setting: it ignores ESC/POS density commands. Continuous paper only: set Paper Type to Normal (black-mark labels print in pieces) |

### TSC

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | DA210 | USB, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | DA220 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TC200 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TDP-225 | USB, Network | tspl/tspl2 |  |
| 🟡 | TDP-244 | USB | tspl/tspl2 |  |
| 🟡 | TDP-247 | USB, Network | tspl/tspl2 |  |
| 🟡 | TE200 | USB, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TE210 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TE300 | USB, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TE310 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TTP-244Pro | USB | tspl/tspl2 |  |
| 🟡 | TX200 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |
| 🟡 | TX300 | USB, Network, Bluetooth Classic | tspl/tspl2 |  |

### Xprinter

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | XP-365B | USB, Network, Bluetooth Classic | tspl |  |
| 🟡 | XP-420B | USB, Network, Bluetooth Classic | tspl |  |
| 🟡 | XP-470B | USB, Bluetooth Classic | tspl |  |
| 🟡 | XP-D463B | USB, Bluetooth Classic | tspl |  |

### Zebra

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | LP2722 | Network | epl/epl2 |  |
| 🟡 | LP2742 | Network | epl/epl2 |  |
| 🟡 | LP2824 | USB, Network, Serial | epl/epl2 |  |
| 🟡 | LP2844 | USB, Network, Serial | epl/epl2 |  |
| 🟡 | TLP2722 | Network | epl/epl2 |  |
| 🟡 | TLP2742 | Network | epl/epl2 |  |
| 🟡 | TLP2746e | USB, Network | epl/epl2 |  |
| 🟡 | TLP2824 | USB, Network, Serial | epl/epl2 |  |
| 🟡 | TLP2844 | USB, Network, Serial | epl/epl2 |  |
| 🟡 | TLP3742 | Network | epl/epl2 |  |
| 🟡 | TLP3842 | USB, Network, Serial | epl/epl2 |  |
| 🟡 | GC420d | Network, USB, Serial | zpl |  |
| 🟡 | GC420t | Network, USB, Serial | zpl |  |
| ✅ community | GK420d | Network, USB, Serial | zpl | tested by a friend of the maintainer, 2026-11 (ZPL over TCP) |
| 🟡 | GK420t | Network, USB, Serial | zpl |  |
| 🟡 | GX420d | Network, USB, Serial | zpl |  |
| 🟡 | GX420t | Network, USB, Serial | zpl |  |
| 🟡 | GX430t | Network, USB, Serial | zpl |  |
| 🟡 | LP2844-Z | Network, USB, Serial | zpl |  |
| 🟡 | ZD220 | Network, USB | zpl |  |
| 🟡 | ZD230 | Network, USB | zpl |  |
| 🟡 | ZD410 | Network, USB | zpl |  |
| 🟡 | ZD420-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZD420-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZD421-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZD421-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZD620-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZD620-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZD621-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZD621-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZP450 | Network, USB | zpl |  |
| 🟡 | ZP500 | Network, USB | zpl |  |
| 🟡 | ZQ310 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ320 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ510 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ511 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ520 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ521 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ610 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ620 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZQ630 | Network, USB, Bluetooth Classic | zpl |  |
| 🟡 | ZT230-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT230-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT231-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT231-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT410-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT410-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT411-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT411-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT420-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT420-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT421-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT421-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT510-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT510-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT610-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT610-300 | Network, USB, Serial | zpl |  |
| 🟡 | ZT620-203 | Network, USB, Serial | zpl |  |
| 🟡 | ZT620-300 | Network, USB, Serial | zpl |  |

<!-- printers:end -->

## Not supported yet

| | Printer | Why |
|---|---|---|
| ⏳ | Brother QL-500, QL-550 | No `ESC i A`/`ESC i K` commands; the QL-500 has no cutter. |
| ⏳ | Brother QL-650TD | No `ESC i A`; compression only over the serial port. |
| ⏳ | Brother QL-600 | Needs `ESC i a FF` after every job. |
| ⏳ | Brother PT-D600 | Brother publishes no raster command reference for it. |
| ⏳ | Brother PT-H500 | Unclear how its cutter is driven. |
| ⏳ | DYMO LabelWriter SE450 | 203 dpi with its own command set; needs a driver. |
| ⏳ | DYMO LabelWriter 300, 310, 320 | Serial-only, and no technical reference found (head width, commands). |
| ⏳ | DYMO LabelWriter Duo tape side (D1) | A different tape protocol; the label side works. |
| ⏳ | Zebra LP 2622, LP 2443, LP 2684 Strata (EPL) | Serial or parallel only; no spec sheet with head width and dpi found. |
| ⏳ | Zebra LP 2442, LP 2348 (EPL) | No programming reference found. |
| ⏳ | Niimbot B3S, D110_M, B21_L2B, H1S, K3, B203 | Protocol variants not covered by public write-ups yet (B3S Bluetooth is reported broken). |
| ⏳ | Phomemo M02X, D50, P12, M03, M04S | Different pacing, 180 dpi, no print-done reply, or no permissively licensed reference. |

## Can't be supported

| | Printer | Why |
|---|---|---|
| ❌ | DYMO LabelWriter 5xx series | Labels are chip-locked and the protocol is undocumented. |
| ❌ | Printers that only work through a vendor cloud app | There is no local connection to print over. |

## Test an untested printer

A 🟡 printer becomes ✅ when someone has printed with it. If you own one:

1. `pdb-print discover` finds printers on USB, the network and Bluetooth. It only asks, never prints.
2. Set it up: in the web UI (`pdb-print serve`), "Add a printer" runs the setup wizard, which finds the
   printer, prints a test label and asks how it came out.
   For Zebra and Bluetooth cat printers there is also `pdb-print try [HOST]`, a guided test with numbered
   test labels and questions; it writes `pdb-print-try-<model>-<time>.txt`.
3. `pdb-print report` writes `pdb-print-report-<time>.json` with what pdb-print found out about the printer.
   `--identify` adds the replies to each printer language's identify query, `--print` a test label (both ask
   first). Addresses and serial numbers are masked; check the file before you post it.
4. [Open a GitHub issue](https://github.com/francisrath/pdb-print/issues/new) with:
   - the report file (and the `try` file, if you ran it);
   - a photo of the test card or label;
   - the printer model and firmware version (often on a status print or in its web page);
   - what worked and what didn't.

## Supporting a new printer

For a printer that isn't listed, open an issue with:

- the exact model name;
- a link to a public command reference or programming manual, or at least the spec sheet;
- its USB id (`lsusb` on Linux, or `pdb-print report usb`);
- its network or Bluetooth name;
- the label sizes it takes;
- ideally, a printer to test with (yours, or see [below](#donate-a-printer)).

A model of a family pdb-print already speaks (another Brother QL, a ZPL Zebra, a cat printer clone) is
a data change: one entry in [`internal/devicedb/data/`](../internal/devicedb/data/). For example the
QL-820NWB in `brother-ql.yaml` differs from the QL-810W only in its id and in how it is recognised
(status code, USB id, IEEE 1284 and SNMP names). A new
family needs a driver under `internal/driver/`; see "Adding a printer" in the [README](../README.md#adding-a-printer).

## Donate a printer

Printer donations for testing are welcome, new or used. [Open a GitHub issue](https://github.com/francisrath/pdb-print/issues/new)
to get in touch about the address and details.
