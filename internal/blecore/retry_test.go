package blecore

import (
	"context"
	"errors"
	"sync/atomic"
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

func TestConnected(t *testing.T) {
	type device struct{ p *int }
	boom := errors.New("boom")
	n := 1
	for _, tc := range []struct {
		name string
		dev  device
		err  error
		ok   bool
	}{
		{"connected", device{&n}, nil, true},
		{"error", device{}, boom, false},
		{"zero device without error (early refusal)", device{}, nil, false},
	} {
		err := Connected(tc.dev, tc.err)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v", tc.name, err)
		}
		if tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.err)
		}
	}
}

func TestDial(t *testing.T) {
	ctx := context.Background()
	t.Run("connects", func(t *testing.T) {
		d, err := Dial(ctx, time.Second, time.Second, func() (int, error) { return 1, nil }, func(int) { t.Error("dropped") })
		if d != 1 || err != nil {
			t.Fatalf("%v %v", d, err)
		}
	})
	t.Run("late connection is dropped before Dial returns", func(t *testing.T) {
		var dropped atomic.Int32
		_, err := Dial(ctx, 20*time.Millisecond, time.Second, func() (int, error) {
			time.Sleep(60 * time.Millisecond)
			return 7, nil
		}, func(d int) { dropped.Store(int32(d)) })
		if !errors.Is(err, ErrConnectTimeout) {
			t.Fatalf("err %v", err)
		}
		if dropped.Load() != 7 {
			t.Fatal("the late connection was still up when Dial returned")
		}
	})
	t.Run("an attempt that never ends is left after grace", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		start := time.Now()
		_, err := Dial(ctx, 10*time.Millisecond, 30*time.Millisecond, func() (int, error) { <-release; return 0, errors.New("x") }, func(int) {})
		if !errors.Is(err, ErrConnectTimeout) || time.Since(start) > time.Second {
			t.Fatalf("err %v after %v", err, time.Since(start))
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := Dial(ctx, time.Second, time.Second, func() (int, error) { time.Sleep(20 * time.Millisecond); return 0, errors.New("x") }, func(int) {}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	})
}
