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
		wait        time.Duration // generous unless the case is about running out of time
		errs        []error       // per call; the last repeats
		calls       int
		noBluetooth bool
		want        string
	}{
		{"on", 5 * time.Second, []error{nil}, 1, false, ""},
		{"prompt answered", 5 * time.Second, []error{timeout, timeout, nil}, 3, false, ""},
		{"off", 5 * time.Second, []error{off}, 1, true, "powered off"},
		{"prompt left open", 35 * time.Millisecond, []error{timeout}, 0, false, "if macOS asked for permission, allow it and run again; otherwise check that Bluetooth is on"},
	} {
		calls := 0
		err := WaitEnable(func() error {
			calls++
			time.Sleep(10 * time.Millisecond)
			return tc.errs[min(calls, len(tc.errs))-1]
		}, tc.wait)
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
