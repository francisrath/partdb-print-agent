# partdb-print-agent

Direct label printing for [Part-DB](https://github.com/Part-DB/Part-DB-server).

A small cross-platform agent (`pdb-print`) that takes the label PDFs Part-DB already generates,
rasterises them at the printer's native resolution and sends them straight to your label printer –
crisp QR codes, exact sizes, automatic cutting, no fiddling with OS print dialogs.

**Status:** early development. First target: Brother QL printers over the network.

- Works with Part-DB as it is today (read-only API token).
- Planned: Zebra/ZPL, TSPL, ESC/POS, DYMO, Niimbot, Phomemo, normal printers and A4 label sheets;
  instant printing from any device through NAT via an optional Part-DB extension.

See [`docs/architecture.md`](docs/architecture.md) for the full design.

## License
MIT
