package blecore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConnectRetry(t *testing.T) {
	abort, timeout := errors.New("le-connection-abort-by-local"), errors.New("timeout")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		errs    []error // per attempt
		calls   int
		resets  int
		wantErr error
	}{
		{"first try", context.Background(), []error{nil}, 1, 0, nil},
		{"abort, then ok", context.Background(), []error{abort, nil}, 2, 1, nil},
		{"abort twice", context.Background(), []error{abort, abort}, 2, 1, abort},
		{"timeout: no retry", context.Background(), []error{timeout}, 1, 0, timeout},
		{"cancelled: no retry", cancelled, []error{abort}, 1, 0, abort},
	} {
		var calls, resets int
		var resetAt time.Time
		err := ConnectRetry(tc.ctx, 30*time.Millisecond, func() error {
			calls++
			if calls == 2 && (resetAt.IsZero() || time.Since(resetAt) < 30*time.Millisecond) {
				t.Errorf("%s: retry without reset and pause", tc.name)
			}
			return tc.errs[calls-1]
		}, func(err error) bool { return err != timeout }, func() { resets++; resetAt = time.Now() })
		if calls != tc.calls || resets != tc.resets || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
			t.Errorf("%s: %d calls, %d resets, err %v", tc.name, calls, resets, err)
		}
	}
}
