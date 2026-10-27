package taginbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/francisrath/partdb-print-agent/internal/partdb"
)

type harness struct {
	in      *Inbox
	tagged  []partdb.TaggedPart
	printed [][]int
	failing bool
}

func newHarness(t *testing.T) *harness {
	h := &harness{}
	h.in = &Inbox{Key: "home/print-label", State: filepath.Join(t.TempDir(), "taginbox.json"),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fetch: func(context.Context) ([]partdb.TaggedPart, error) { return h.tagged, nil },
		Print: func(_ context.Context, ids []int) error {
			if h.failing {
				return errors.New("printer offline")
			}
			h.printed = append(h.printed, ids)
			return nil
		}}
	return h
}

func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.in.poll(context.Background()); err != nil && !h.failing {
		t.Fatal(err)
	}
}

func tp(id int, mod string) partdb.TaggedPart { return partdb.TaggedPart{ID: id, LastModified: mod} }

func TestFirstRunRecordsBacklogWithoutPrinting(t *testing.T) {
	h := newHarness(t)
	h.tagged = []partdb.TaggedPart{tp(1, "a"), tp(2, "a")}
	h.poll(t)
	h.poll(t)
	if len(h.printed) != 0 {
		t.Errorf("backlog printed: %v", h.printed)
	}
}

func TestPrintsNewlyTaggedOnceAndReprintsOnChange(t *testing.T) {
	h := newHarness(t)
	h.poll(t) // first run, nothing tagged
	h.tagged = []partdb.TaggedPart{tp(32, "t1"), tp(5, "t1")}
	h.poll(t)
	h.poll(t)
	if !reflect.DeepEqual(h.printed, [][]int{{5, 32}}) {
		t.Fatalf("printed %v", h.printed)
	}
	h.tagged = []partdb.TaggedPart{tp(32, "t2"), tp(5, "t1")} // part 32 re-tagged/edited
	h.poll(t)
	if !reflect.DeepEqual(h.printed[1:], [][]int{{32}}) {
		t.Errorf("reprint: %v", h.printed)
	}
}

func TestUntagThenTagPrintsAgain(t *testing.T) {
	h := newHarness(t)
	h.poll(t)
	h.tagged = []partdb.TaggedPart{tp(7, "t1")}
	h.poll(t)
	h.tagged = nil // tag removed
	h.poll(t)
	h.tagged = []partdb.TaggedPart{tp(7, "t1")} // same lastModified, tagged again
	h.poll(t)
	if len(h.printed) != 2 {
		t.Errorf("printed %v", h.printed)
	}
}

func TestFailedPrintIsRetriedAndStateSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.poll(t)
	h.tagged = []partdb.TaggedPart{tp(9, "t1")}
	h.failing = true
	h.poll(t)
	h.failing = false
	h.poll(t) // retry succeeds
	// a new Inbox on the same state file (agent restart) doesn't reprint
	h2 := newHarness(t)
	h2.in.State, h2.tagged = h.in.State, h.tagged
	h2.poll(t)
	if !reflect.DeepEqual(h.printed, [][]int{{9}}) || len(h2.printed) != 0 {
		t.Errorf("printed %v, after restart %v", h.printed, h2.printed)
	}
}

func TestBatchesLargeBursts(t *testing.T) {
	h := newHarness(t)
	h.poll(t)
	for i := 1; i <= maxPerPoll+5; i++ {
		h.tagged = append(h.tagged, tp(i, "t"))
	}
	h.poll(t)
	h.poll(t)
	if len(h.printed) != 2 || len(h.printed[0]) != maxPerPoll || len(h.printed[1]) != 5 {
		t.Errorf("batches: %d, %v", len(h.printed), h.printed)
	}
}
