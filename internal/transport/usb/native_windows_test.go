package usb

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOverlappedResult(t *testing.T) {
	aborted := windows.ERROR_OPERATION_ABORTED
	for _, c := range []struct {
		name         string
		n            uint32
		err          error
		cancelled    bool
		wantN        int
		wantDeadline bool
		wantErr      bool
	}{
		{"done", 5, nil, false, 5, false, false},
		{"our cancel, nothing moved", 0, aborted, true, 0, true, false},
		{"our cancel, some bytes moved", 3, aborted, true, 3, false, false},
		{"aborted without our cancel (unplugged)", 0, aborted, false, 0, false, true},
		{"other error", 0, windows.ERROR_DEVICE_NOT_CONNECTED, false, 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, err := overlappedResult(c.n, c.err, c.cancelled)
			if n != c.wantN || errors.Is(err, os.ErrDeadlineExceeded) != c.wantDeadline || (err != nil && !c.wantDeadline) != c.wantErr {
				t.Errorf("got %d, %v", n, err)
			}
		})
	}
}
