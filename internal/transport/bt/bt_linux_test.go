package bt

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

var osErrnos = linuxErrnos

var osCases = []osCase{
	{unix.EHOSTDOWN, true, false},
	{unix.ECONNREFUSED, true, false},
	{unix.ETIMEDOUT, true, false},
	{unix.EHOSTUNREACH, false, true},
	{unix.EADDRNOTAVAIL, false, true},
	{unix.ENODEV, false, true},
	{unix.EAFNOSUPPORT, false, true},
	{unix.EACCES, true, false},
	{unix.ENOMEM, true, false},
	{unix.EBUSY, true, false},
	{unix.EPROTO, false, false},
}

func TestRetryErrnos(t *testing.T) {
	for _, e := range []unix.Errno{unix.ENOMEM, unix.EBUSY} {
		if !isAny(e, linuxErrnos.retry) {
			t.Errorf("%v not retried", e)
		}
	}
}

// A cancelled Open is ctx.Err(), not "no Bluetooth", even when the name lookup is what fails.
func TestDialCancelledDuringLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "", "RG-MTP80B", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial = %v, want context.Canceled", err)
	}
}
