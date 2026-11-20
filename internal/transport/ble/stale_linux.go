package ble

import (
	"strings"

	"github.com/godbus/dbus/v5"
	"tinygo.org/x/bluetooth"
)

// dropStale asks BlueZ to drop a connection it may still hold to addr (what `bluetoothctl disconnect`
// does). tinygo bluetooth v0.17.0 hands out a Device only after a successful Connect, so this calls
// Device1.Disconnect on BlueZ's object for the default adapter (hci0, as the library) itself. Errors
// (no such device, not connected) don't matter: the retry follows anyway.
func dropStale(addr bluetooth.Address) {
	bus, err := dbus.SystemBus() // the library's shared connection
	if err != nil {
		return
	}
	path := dbus.ObjectPath("/org/bluez/hci0/dev_" + strings.ReplaceAll(addr.MAC.String(), ":", "_"))
	_ = bus.Object("org.bluez", path).Call("org.bluez.Device1.Disconnect", 0).Err
}
