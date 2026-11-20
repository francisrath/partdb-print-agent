package blecore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// NotFound adds what to check to a scan that missed the printer. A printer still connected (perhaps
// from the last job) doesn't advertise.
func NotFound(err error, within time.Duration) error {
	return fmt.Errorf("%w within %v (is it on and not connected to a phone? If it is still connected from the last job, switch it off and on)", err, within)
}

// How long the macOS helper's Close waits for notifications to be switched off and for the disconnect
// to be confirmed; its client gives it longer before killing it.
const (
	NotifyOffWait  = time.Second
	DisconnectWait = 2 * time.Second
)

// Link tells the macOS helper's connection-state handler our own disconnect (after Closing) from a
// dropped link, which ends the job.
type Link struct {
	closing atomic.Bool
	gone    chan struct{}
	once    sync.Once
}

func NewLink() *Link { return &Link{gone: make(chan struct{})} }

// Closing marks the disconnects from now on as ours.
func (l *Link) Closing() { l.closing.Store(true) }

// Dropped is called for each disconnect; it reports whether the drop is fatal (not ours).
func (l *Link) Dropped() bool {
	if !l.closing.Load() {
		return true
	}
	l.once.Do(func() { close(l.gone) })
	return false
}

// Wait reports whether our disconnect was confirmed within d.
func (l *Link) Wait(d time.Duration) bool {
	select {
	case <-l.gone:
		return true
	case <-time.After(d):
		return false
	}
}
