package devicedb

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

var transportNames = map[string]string{"tcp": "Network", "usb": "USB", "ble": "Bluetooth LE", "bt": "Bluetooth Classic"}

// MarkdownMatrix renders the support matrix of docs/printers.md: a table per vendor, one row per
// model (by driver, dialect, id) with its tested status, connections, driver and note.
func (db *DB) MarkdownMatrix() string {
	models := make([]*Model, 0, len(db.models))
	for _, m := range db.models {
		models = append(models, m)
	}
	slices.SortFunc(models, func(a, b *Model) int {
		return cmp.Or(cmp.Compare(a.Vendor, b.Vendor), cmp.Compare(a.Driver, b.Driver), cmp.Compare(a.Dialect, b.Dialect), cmp.Compare(a.ID, b.ID))
	})
	var b strings.Builder
	for i, m := range models {
		if i == 0 || m.Vendor != models[i-1].Vendor {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "### %s\n\n| Status | Model | Connections | Driver | Notes |\n|---|---|---|---|---|\n", cmp.Or(m.Vendor, "Other"))
		}
		status := "🟡"
		if m.Tested != "" {
			status = "✅ " + m.Tested
		}
		conns := make([]string, len(m.Transports))
		for j, t := range m.Transports {
			conns[j] = cmp.Or(transportNames[t], t)
		}
		drv := m.Driver
		if m.Dialect != m.Driver {
			drv += "/" + m.Dialect
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", status, m.ID, strings.Join(conns, ", "), drv, strings.ReplaceAll(m.TestedNote, "|", `\|`))
	}
	return b.String()
}
