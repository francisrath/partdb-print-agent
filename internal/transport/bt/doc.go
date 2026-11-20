// Package bt is the Bluetooth Classic transport (kind "bt"): one RFCOMM (Serial Port Profile) connection
// to a paired printer. Linux talks to the kernel's RFCOMM sockets and asks BlueZ (D-Bus) for names;
// Windows uses Winsock's AF_BTH sockets. The OS files only gather devices and dial; the Transport, name
// resolution and error mapping in bt.go are shared and tested on every OS.
package bt
