package blecore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLink(t *testing.T) {
	l := NewLink()
	if !l.Dropped() {
		t.Error("a drop before Closing isn't fatal")
	}
	if l.Wait(10 * time.Millisecond) {
		t.Error("Wait saw a disconnect that wasn't ours")
	}
	l.Closing()
	for range 2 { // a second callback must not panic or block
		if l.Dropped() {
			t.Error("our own disconnect is reported as fatal")
		}
	}
	if !l.Wait(time.Second) {
		t.Error("Wait missed the disconnect")
	}
}

func TestNotFoundHint(t *testing.T) {
	err := NotFound(fmt.Errorf("%w: no device named %q", ErrNotFound, "MX10"), 10*time.Second)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("%v doesn't wrap ErrNotFound", err)
	}
	for _, want := range []string{"within 10s", "not connected to a phone", "still connected from the last job", "switch it off and on"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
}
