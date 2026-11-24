package usb

import "testing"

func TestUSBIDSerial(t *testing.T) {
	for _, c := range []struct{ name, id, parent, usbID, serial string }{
		{"composite: serial on the parent", `USB\VID_04F9&PID_209C&MI_00\7&94CFD79&1&0000`, `USB\VID_04F9&PID_209C\A1B2C3`, "04f9:209c", "A1B2C3"},
		{"plain: serial on the device", `USB\VID_04F9&PID_200F\A1B2C3`, `USB\VID_8087&PID_0024\5&1515240A&0&1`, "04f9:200f", "A1B2C3"},
		{"no serial: generated id", `USB\VID_0922&PID_0020\6&2B0CA1B2&0&3`, "", "0922:0020", ""},
		{"lowercase", `usb\vid_04f9&pid_209c&mi_00\7&1&0`, `usb\vid_04f9&pid_209c\x9`, "04f9:209c", "x9"},
		{"not usb", `HID\FOO`, "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if id, s := usbIDSerial(c.id, c.parent); id != c.usbID || s != c.serial {
				t.Errorf("got %q %q, want %q %q", id, s, c.usbID, c.serial)
			}
		})
	}
}

func TestParse1284Reply(t *testing.T) {
	for in, want := range map[string]string{
		"\x8d\x8dMFG:Brother;MDL:QL-810W;\x00": "MFG:Brother;MDL:QL-810W;",
		"\x33\x33MFG:Brother;CMD:PT-CBP; ":     "MFG:Brother;CMD:PT-CBP;",
		"\x00":                                 "",
		"":                                     "",
	} {
		if got := parse1284Reply([]byte(in)); got != want {
			t.Errorf("parse1284Reply(%q) = %q, want %q", in, got, want)
		}
	}
}
