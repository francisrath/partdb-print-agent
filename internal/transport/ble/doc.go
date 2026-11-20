// Package ble is the Bluetooth LE transport (kind "ble") for the cat printers. On Linux and Windows it
// talks to the printer in-process (tinygo.org/x/bluetooth); on macOS the kind is not registered yet
// (pdb-print never imports the Bluetooth library there – a separate helper will do the talking).
package ble
