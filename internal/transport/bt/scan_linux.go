package bt

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

const sppUUIDString = "00001101-0000-1000-8000-00805f9b34fb"

// Scan runs a Bluetooth Classic inquiry of timeout on the first adapter, then lists every classic device
// BlueZ knows (found now, paired or remembered). It uses a D-Bus connection of its own: BlueZ keeps one
// discovery session and filter per client, and the BLE scan uses the shared connection at the same time.
func Scan(ctx context.Context, timeout time.Duration) ([]Device, error) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, bluezErr(ctx, "D-Bus", err)
	}
	defer bus.Close() // also ends the discovery session if StopDiscovery didn't
	objs, err := managedObjects(ctx, bus)
	if err != nil {
		return nil, err
	}
	var adapters []dbus.ObjectPath
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.Adapter1"]; ok {
			adapters = append(adapters, path)
		}
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("bt: %w: BlueZ has no adapter", blecore.ErrNoBluetooth)
	}
	slices.Sort(adapters)
	a := bus.Object("org.bluez", adapters[0])
	filter := map[string]dbus.Variant{"Transport": dbus.MakeVariant("bredr")}
	if err := a.CallWithContext(ctx, "org.bluez.Adapter1.SetDiscoveryFilter", 0, filter).Err; err != nil {
		return nil, bluezErr(ctx, "BlueZ", err)
	}
	if err := a.CallWithContext(ctx, "org.bluez.Adapter1.StartDiscovery", 0).Err; err != nil {
		return nil, bluezErr(ctx, "BlueZ", err) // org.bluez.Error.NotReady: the adapter is off
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // bus.Close covers the rest
	_ = a.CallWithContext(stopCtx, "org.bluez.Adapter1.StopDiscovery", 0).Err
	cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if objs, err = managedObjects(ctx, bus); err != nil {
		return nil, err
	}
	return classicDevices(objs), nil
}

// classicDevices are the Device1 objects with a class of device or the SPP UUID (BR/EDR), by address; a device known to
// two adapters is one row (the first adapter's, with SPP and the class if either has them).
func classicDevices(objs bluezObjects) []Device {
	paths := slices.Sorted(maps.Keys(objs))
	var devs []Device
	for _, path := range paths {
		props, ok := objs[path]["org.bluez.Device1"]
		if !ok {
			continue
		}
		addr, _ := props["Address"].Value().(string)
		class, hasClass := props["Class"].Value().(uint32)
		uuids, _ := props["UUIDs"].Value().([]string)
		spp := slices.ContainsFunc(uuids, func(u string) bool { return strings.EqualFold(u, sppUUIDString) })
		if addr == "" || !hasClass && !spp {
			continue // LE only (BlueZ's Class is optional: the serial port alone also says Classic)
		}
		name, _ := props["Name"].Value().(string)
		if name == "" {
			name, _ = props["Alias"].Value().(string)
		}
		if i := slices.IndexFunc(devs, func(d Device) bool { return strings.EqualFold(d.Address, addr) }); i >= 0 {
			devs[i].SPP = devs[i].SPP || spp
			devs[i].Class = cmp.Or(devs[i].Class, class)
			continue
		}
		devs = append(devs, Device{Name: name, Address: addr, Class: class, SPP: spp})
	}
	slices.SortFunc(devs, func(x, y Device) int { return cmp.Compare(x.Address, y.Address) })
	return devs
}
