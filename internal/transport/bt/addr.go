package bt

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ParseAddr reads a Bluetooth address written AA:BB:CC:DD:EE:FF (any case), most significant byte first.
func ParseAddr(s string) ([6]byte, error) {
	var a [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return a, fmt.Errorf("bt: address %q must look like AA:BB:CC:DD:EE:FF", s)
	}
	for i, p := range parts {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) != 1 {
			return [6]byte{}, fmt.Errorf("bt: address %q must look like AA:BB:CC:DD:EE:FF", s)
		}
		a[i] = b[0]
	}
	return a, nil
}

// FormatAddr writes a as AA:BB:CC:DD:EE:FF.
func FormatAddr(a [6]byte) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[0], a[1], a[2], a[3], a[4], a[5])
}

// rfcommAddr is a in BlueZ's sockaddr_rc order (little-endian: least significant byte first).
func rfcommAddr(a [6]byte) [6]byte {
	return [6]byte{a[5], a[4], a[3], a[2], a[1], a[0]}
}

// bthAddr is a as Windows' BTH_ADDR: AA:BB:CC:DD:EE:FF is 0xAABBCCDDEEFF.
func bthAddr(a [6]byte) uint64 {
	var v uint64
	for _, b := range a {
		v = v<<8 | uint64(b)
	}
	return v
}

// fromBth is the inverse of bthAddr.
func fromBth(v uint64) [6]byte {
	var a [6]byte
	for i := 5; i >= 0; i-- {
		a[i] = byte(v)
		v >>= 8
	}
	return a
}
