package ble

import (
	"errors"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/blecore"
)

func TestScanError(t *testing.T) {
	tests := []struct {
		msg         string
		noBluetooth bool
	}{
		{"bluetooth: adaptor is not powered", true},        // Linux, adapter off
		{"failed to stop scanning (error code 1)", true},   // Windows RadioNotAvailable
		{"failed to stop scanning (error code 5)", true},   // DisabledByPolicy
		{"failed to stop scanning (error code 6)", true},   // NotSupported (no LE)
		{"failed to stop scanning (error code 7)", true},   // DisabledByUser
		{"failed to stop scanning (error code 2)", false},  // ResourceInUse
		{"failed to stop scanning (error code 10)", false}, // not code 1
		{"org.bluez.Error.InProgress: Operation already in progress", false},
		{"bluetooth: scan stopped unexpectedly", false},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			lib := errors.New(tt.msg)
			err := scanError(lib)
			if !errors.Is(err, lib) {
				t.Fatalf("%v does not wrap the library error", err)
			}
			if got := errors.Is(err, blecore.ErrNoBluetooth); got != tt.noBluetooth {
				t.Fatalf("%v: ErrNoBluetooth %v, want %v", err, got, tt.noBluetooth)
			}
		})
	}
}
