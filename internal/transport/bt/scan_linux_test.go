package bt

import (
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestClassicDevices(t *testing.T) {
	v := dbus.MakeVariant
	objs := bluezObjects{
		"/org/bluez/hci0": {"org.bluez.Adapter1": {"Address": v("00:11:22:33:44:55")}},
		"/org/bluez/hci0/dev_02": {"org.bluez.Device1": {"Address": v("AA:BB:CC:DD:EE:02"), "Alias": v("Receipt"),
			"Class": v(uint32(0x0680))}},
		"/org/bluez/hci0/dev_01": {"org.bluez.Device1": {"Address": v("AA:BB:CC:DD:EE:01"), "Name": v("RG-MLP80B"), "Alias": v("Bench"),
			"Class": v(uint32(0x0680)), "UUIDs": v([]string{"00001101-0000-1000-8000-00805F9B34FB"})}},
		"/org/bluez/hci0/dev_03": {"org.bluez.Device1": {"Address": v("AA:BB:CC:DD:EE:03"), "Name": v("MX10")}}, // LE: no class
		// no class (BlueZ's Class is optional), but it offers the serial port: Classic
		"/org/bluez/hci0/dev_04": {"org.bluez.Device1": {"Address": v("AA:BB:CC:DD:EE:04"), "Name": v("RG-MLP80B"),
			"UUIDs": v([]string{"00001101-0000-1000-8000-00805f9b34fb"})}},
		// the same printer known to a second adapter: one row
		"/org/bluez/hci1/dev_01": {"org.bluez.Device1": {"Address": v("aa:bb:cc:dd:ee:01"), "Name": v("RG-MLP80B"),
			"Class": v(uint32(0x0680))}},
	}
	want := []Device{
		{Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:01", Class: 0x0680, SPP: true},
		{Name: "Receipt", Address: "AA:BB:CC:DD:EE:02", Class: 0x0680},
		{Name: "RG-MLP80B", Address: "AA:BB:CC:DD:EE:04", SPP: true},
	}
	if got := classicDevices(objs); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
