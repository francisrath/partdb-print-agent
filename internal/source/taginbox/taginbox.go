// Package taginbox prints labels for Part-DB parts that carry a tag (architecture §4.1,
// source 4). Tag a part on any device and the agent prints it — it only polls outbound,
// so it works through NAT. With a read-only token the tag can't be removed, so the
// inbox remembers locally which (part, lastModified) pairs it printed.
package taginbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/partdb"
)

const maxPerPoll = 20 // a lost state file or mass-tagging can't flood the printer

type Inbox struct {
	Key      string        // state key, e.g. "<instance>/<tag>"
	Interval time.Duration // default 10 s
	State    string        // JSON file remembering printed parts
	Fetch    func(ctx context.Context) ([]partdb.TaggedPart, error)
	Print    func(ctx context.Context, ids []int) error
	Log      *slog.Logger
}

// state maps inbox key → part id → lastModified that was printed (or skipped).
type state map[string]map[int]string

// Run polls until ctx is done. After a failed fetch or print the wait doubles, up to 5 min.
func (in *Inbox) Run(ctx context.Context) error {
	if in.Interval == 0 {
		in.Interval = 10 * time.Second
	}
	wait := in.Interval
	for {
		if err := in.poll(ctx); err != nil {
			in.Log.Error("tag inbox", "err", err)
			wait = min(wait*2, 5*time.Minute)
		} else {
			wait = in.Interval
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (in *Inbox) poll(ctx context.Context) error {
	parts, err := in.Fetch(ctx)
	if err != nil {
		return err
	}
	st, err := in.load()
	if err != nil {
		return err
	}
	done, first := st[in.Key], st[in.Key] == nil
	current := map[int]string{}
	var todo []int
	for _, p := range parts {
		current[p.ID] = p.LastModified
		if !first && done[p.ID] != p.LastModified {
			todo = append(todo, p.ID)
		}
	}
	if first {
		// first run for this tag: don't flood the printer with an old backlog
		st[in.Key] = current
		if len(parts) > 0 {
			in.Log.Info("tag inbox: parts already tagged were recorded, not printed; re-tag or edit them to print", "count", len(parts))
		}
		return in.save(st)
	}
	// forget parts that lost the tag, so tagging them again prints them
	for id := range done {
		if _, ok := current[id]; !ok {
			delete(done, id)
		}
	}
	sort.Ints(todo)
	if len(todo) > maxPerPoll {
		in.Log.Warn("tag inbox: many new parts, printing in batches", "pending", len(todo), "batch", maxPerPoll)
		todo = todo[:maxPerPoll]
	}
	if len(todo) > 0 {
		if err := in.Print(ctx, todo); err != nil {
			return err // nothing recorded: retried after the backoff
		}
		in.Log.Info("tag inbox: printed", "parts", todo)
		for _, id := range todo {
			done[id] = current[id]
		}
	}
	return in.save(st)
}

func (in *Inbox) load() (state, error) {
	b, err := os.ReadFile(in.State)
	if errors.Is(err, fs.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := state{}
	return st, json.Unmarshal(b, &st)
}

// save writes the state atomically, so a crash can't leave it half-written.
func (in *Inbox) save(st state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(in.State), 0o700); err != nil {
		return err
	}
	tmp := in.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, in.State)
}
