package usb

import "strings"

// usbIDSerial reads the USB id and serial from a Windows device instance ID
// (USB\VID_04F9&PID_200F\<serial>). The function of a composite device (…&MI_00\…) has neither:
// they are on its parent, the device itself. Windows makes up an instance ID with "&" in it when
// the device has no serial; that gives "".
func usbIDSerial(instanceID, parentID string) (usbID, serial string) {
	id := instanceID
	if strings.Contains(strings.ToUpper(id), "&MI_") {
		id = parentID
	}
	parts := strings.Split(id, `\`)
	if len(parts) != 3 || !strings.EqualFold(parts[0], "USB") {
		return "", ""
	}
	hw := strings.ToLower(parts[1])
	if !strings.HasPrefix(hw, "vid_") || len(hw) < 17 || hw[8:13] != "&pid_" {
		return "", ""
	}
	usbID = hw[4:8] + ":" + hw[13:17]
	if !strings.Contains(parts[2], "&") {
		serial = parts[2]
	}
	return usbID, serial
}

// parse1284Reply returns the device ID from an IOCTL_USBPRINT_GET_1284_ID reply. The reply starts
// with two length bytes, which printers fill inconsistently (a QL-810W sends 8d 8d for 141 bytes),
// so they are skipped rather than trusted.
func parse1284Reply(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	return strings.TrimRight(string(b[2:]), "\x00 ")
}
