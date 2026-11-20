package blecore

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitEnable(t *testing.T) {
	timeout := errors.New("timeout enabling CentralManager")
	off := errors.New("bluetooth is powered off")
	for _, tc := range []struct {
		name        string
		errs        []error // per call; the last repeats
		calls       int
		noBluetooth bool
		want        string
	}{
		{"on", []error{nil}, 1, false, ""},
		{"prompt answered", []error{timeout, timeout, nil}, 3, false, ""},
		{"off", []error{off}, 1, true, "powered off"},
		{"prompt left open", []error{timeout}, 0, false, "if macOS asked for permission, allow it and run again; otherwise check that Bluetooth is on"},
	} {
		calls := 0
		err := WaitEnable(func() error {
			calls++
			time.Sleep(10 * time.Millisecond)
			return tc.errs[min(calls, len(tc.errs))-1]
		}, 35*time.Millisecond)
		if tc.calls != 0 && calls != tc.calls {
			t.Errorf("%s: %d calls, want %d", tc.name, calls, tc.calls)
		}
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrNoBluetooth) != tc.noBluetooth {
			t.Errorf("%s: err %v, ErrNoBluetooth want %v", tc.name, err, tc.noBluetooth)
		}
	}
}
