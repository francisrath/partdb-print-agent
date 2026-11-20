// Package ble is the Bluetooth LE transport (kind "ble") for the cat printers. On Linux and Windows it
// talks to the printer in-process (tinygo.org/x/bluetooth); on macOS pdb-print never imports the Bluetooth
// library and runs the helper pdb-print-ble instead (bridge_darwin.go, protocol in internal/blebridge).
package ble
