package zpl

import "time"

// SetTiming shortens the completion polling for tests.
func SetTiming(poll, notSeen time.Duration) (restore func()) {
	p, n := pollEvery, notSeenAfter
	pollEvery, notSeenAfter = poll, notSeen
	return func() { pollEvery, notSeenAfter = p, n }
}
