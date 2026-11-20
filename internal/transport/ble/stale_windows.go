package ble

import (
	"context"

	"tinygo.org/x/bluetooth"
)

// dropStale: Windows has no stale-link problem seen so far; the retry just pauses.
func dropStale(context.Context, bluetooth.Address) {}
