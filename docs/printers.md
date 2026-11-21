# Printers

Which label printers pdb-print can drive, how well each one is tested, and how to help with the rest.

## Support matrix

- ✅ tested OK: printed with on real hardware, by the **maintainer** or the **community**
- 🟡 implemented, not tested: written from the manufacturer's documentation; nobody has printed with it yet
- ⏳ not supported yet: see [below](#not-supported-yet)
- ❌ can't be supported: see [below](#cant-be-supported)

The tables below are generated from the device database (`internal/devicedb/data/*.yaml`). Don't edit
them by hand: change the data and run `go test ./internal/devicedb -run TestPrintersDoc -update`.
Driver pull requests adding TSPL, DYMO LabelWriter 300/400/450, Zebra EPL2, Niimbot/Phomemo are
in review; their models appear here once merged.

<!-- printers:begin -->

### Brother

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| ✅ maintainer | PT-9500PC | USB | brother/pt-cbp | USB on a Raspberry Pi, 2026-11 |
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
| ✅ maintainer | QL-810W | Network, USB | brother/ql | TCP and SNMP status, 2026-10 |
| 🟡 | QL-820NWB | Network, USB | brother/ql |  |

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

### REGO

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | RG-MLP80B | Bluetooth Classic | escpos/raster |  |

### Zebra

| Status | Model | Connections | Driver | Notes |
|---|---|---|---|---|
| 🟡 | GC420d | Network, USB | zpl |  |
| 🟡 | GC420t | Network, USB | zpl |  |
| ✅ community | GK420d | Network, USB | zpl | tested by a friend of the maintainer, 2026-11 (ZPL over TCP) |
| 🟡 | GK420t | Network, USB | zpl |  |
| 🟡 | GX420d | Network, USB | zpl |  |
| 🟡 | GX420t | Network, USB | zpl |  |
| 🟡 | GX430t | Network, USB | zpl |  |
| 🟡 | LP2844-Z | Network, USB | zpl |  |
| 🟡 | ZD220 | Network, USB | zpl |  |
| 🟡 | ZD230 | Network, USB | zpl |  |
| 🟡 | ZD410 | Network, USB | zpl |  |
| 🟡 | ZD420-203 | Network, USB | zpl |  |
| 🟡 | ZD420-300 | Network, USB | zpl |  |
| 🟡 | ZD421-203 | Network, USB | zpl |  |
| 🟡 | ZD421-300 | Network, USB | zpl |  |
| 🟡 | ZD620-203 | Network, USB | zpl |  |
| 🟡 | ZD620-300 | Network, USB | zpl |  |
| 🟡 | ZD621-203 | Network, USB | zpl |  |
| 🟡 | ZD621-300 | Network, USB | zpl |  |
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
| 🟡 | ZT230-203 | Network, USB | zpl |  |
| 🟡 | ZT230-300 | Network, USB | zpl |  |
| 🟡 | ZT231-203 | Network, USB | zpl |  |
| 🟡 | ZT231-300 | Network, USB | zpl |  |
| 🟡 | ZT410-203 | Network, USB | zpl |  |
| 🟡 | ZT410-300 | Network, USB | zpl |  |
| 🟡 | ZT411-203 | Network, USB | zpl |  |
| 🟡 | ZT411-300 | Network, USB | zpl |  |
| 🟡 | ZT420-203 | Network, USB | zpl |  |
| 🟡 | ZT420-300 | Network, USB | zpl |  |
| 🟡 | ZT421-203 | Network, USB | zpl |  |
| 🟡 | ZT421-300 | Network, USB | zpl |  |
| 🟡 | ZT510-203 | Network, USB | zpl |  |
| 🟡 | ZT510-300 | Network, USB | zpl |  |
| 🟡 | ZT610-203 | Network, USB | zpl |  |
| 🟡 | ZT610-300 | Network, USB | zpl |  |
| 🟡 | ZT620-203 | Network, USB | zpl |  |
| 🟡 | ZT620-300 | Network, USB | zpl |  |

<!-- printers:end -->

## Not supported yet

| | Printer | Why |
|---|---|---|
| ⏳ | Brother QL-500, QL-550 | No `ESC i A`/`ESC i K` commands; the QL-500 has no cutter. |
| ⏳ | Brother QL-650TD | No `ESC i A`; compression only over the serial port. |
| ⏳ | Brother QL-600 | Needs `ESC i a FF` after every job. |
| ⏳ | Brother PT-D600 | Brother publishes no raster command reference for it. |
| ⏳ | Brother PT-H500 | Unclear how its cutter is driven. |

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
4. [Open a GitHub issue](https://github.com/francisrath/partdb-print-agent/issues/new) with:
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

Printer donations for testing are welcome, new or used. [Open a GitHub issue](https://github.com/francisrath/partdb-print-agent/issues/new)
to get in touch about the address and details.
