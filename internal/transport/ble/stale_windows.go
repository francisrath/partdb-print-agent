package ble

import "tinygo.org/x/bluetooth"

// dropStale: Windows has no stale-link problem seen so far; the retry just pauses.
func dropStale(bluetooth.Address) {}
