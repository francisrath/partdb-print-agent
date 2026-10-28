package agentjobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/partdb"
)

// fakePartDB serves the agent API with the given jobs; job 9 is always claimed by someone else.
type fakePartDB struct {
	mu      sync.Mutex
	jobs    string // JSON list returned by the jobs endpoint
	results []string
	noAPI   bool
	poll    int // poll_seconds in the heartbeat answer (default 7)
	failRes int // fail this many result reports with 500
}

func (f *fakePartDB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case f.noAPI:
		http.NotFound(w, r)
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		poll := f.poll
		if poll == 0 {
			poll = 7
		}
		_, _ = fmt.Fprintf(w, `{"agent_id": 1, "poll_seconds": %d}`, poll)
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		_, _ = io.WriteString(w, f.jobs)
	case strings.HasSuffix(r.URL.Path, "/9/claim"):
		w.WriteHeader(http.StatusConflict)
	case strings.HasSuffix(r.URL.Path, "/claim"):
		_, _ = io.WriteString(w, `{}`)
	case strings.HasSuffix(r.URL.Path, "/result") && f.failRes > 0:
		f.failRes--
		w.WriteHeader(http.StatusInternalServerError)
	case strings.HasSuffix(r.URL.Path, "/result"):
		f.results = append(f.results, strings.TrimPrefix(r.URL.Path, "/en/agent-api/v1/jobs/")+" "+string(body))
		w.WriteHeader(http.StatusNoContent)
	}
}

func newLoop(t *testing.T, f *fakePartDB, print func(context.Context, partdb.Job) (string, error)) *Loop {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Loop{
		Client: partdb.New(srv.URL, "tcp_x"), AgentID: "u-1", Name: "pc", Version: "test",
		Printers: func() []partdb.AgentPrinter { return []partdb.AgentPrinter{{ID: "ql"}} },
		Print:    print, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func jobs(js ...string) string { return "[" + strings.Join(js, ",") + "]" }

func j(id int, typ string) string {
	return fmt.Sprintf(`{"id": %d, "type": %q, "printer": "ql", "payload": {"profile_id": 2, "element_type": "part", "element_ids": "1,2"}}`, id, typ)
}

func TestPrintsClaimedJobsAndReportsResults(t *testing.T) {
	f := &fakePartDB{jobs: jobs(j(7, "print"), j(8, "print"), j(9, "print"), j(10, "locate"))}
	var printed []int
	l := newLoop(t, f, func(_ context.Context, jb partdb.Job) (string, error) {
		printed = append(printed, jb.ID)
		if jb.Payload.ProfileID != 2 || jb.Payload.ElementIDs != "1,2" || jb.Printer != "ql" {
			t.Errorf("job: %+v", jb)
		}
		if jb.ID == 8 {
			return "", errors.New("cover open")
		}
		return "2 page(s) printed", nil
	})

	if wait := l.tick(context.Background()); wait != 7*time.Second {
		t.Errorf("wait %v, want the poll_seconds from Part-DB", wait)
	}
	if len(printed) != 2 || printed[0] != 7 || printed[1] != 8 {
		t.Errorf("printed %v; job 9 was claimed elsewhere, job 10 isn't a print job", printed)
	}
	want := []string{
		`7/result {"status":"done","message":"2 page(s) printed"}`,
		`8/result {"status":"failed","message":"cover open"}`,
		`10/result {"status":"failed","message":"unsupported job type \"locate\""}`,
	}
	if strings.Join(f.results, "\n") != strings.Join(want, "\n") {
		t.Errorf("results:\n%s\nwant:\n%s", strings.Join(f.results, "\n"), strings.Join(want, "\n"))
	}
}

func TestWithoutAgentAPIWaitsLong(t *testing.T) {
	l := newLoop(t, &fakePartDB{noAPI: true}, func(context.Context, partdb.Job) (string, error) {
		t.Error("printed without an agent API")
		return "", nil
	})
	if wait := l.tick(context.Background()); wait != time.Hour {
		t.Errorf("wait %v", wait)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	l := newLoop(t, &fakePartDB{jobs: "[]"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- l.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

func TestShutdownDoesNotInterruptAClaimedJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakePartDB{jobs: jobs(j(7, "print"))}
	l := newLoop(t, f, func(pctx context.Context, _ partdb.Job) (string, error) {
		cancel() // SIGTERM while the label is being printed
		if pctx.Err() != nil {
			return "", pctx.Err()
		}
		return "printed", nil
	})
	l.tick(ctx)
	if want := `7/result {"status":"done","message":"printed"}`; len(f.results) != 1 || f.results[0] != want {
		t.Errorf("results %q, want %q: a label that printed must not be reported as failed", f.results, want)
	}
}

func TestLostResultReportsAreRetried(t *testing.T) {
	f := &fakePartDB{jobs: jobs(j(7, "print")), failRes: 1}
	printed := 0
	l := newLoop(t, f, func(context.Context, partdb.Job) (string, error) { printed++; return "ok", nil })
	l.tick(context.Background())
	f.jobs = "[]" // claimed, so no longer pending
	l.tick(context.Background())
	if printed != 1 || len(f.results) != 1 || f.results[0] != `7/result {"status":"done","message":"ok"}` {
		t.Errorf("printed %d, results %q", printed, f.results)
	}
}

func TestPollIntervalIsClamped(t *testing.T) {
	for poll, want := range map[int]time.Duration{1: 5 * time.Second, 86400: 10 * time.Minute, 60: time.Minute} {
		l := newLoop(t, &fakePartDB{jobs: "[]", poll: poll}, nil)
		if got := l.tick(context.Background()); got != want {
			t.Errorf("poll_seconds %d: wait %v, want %v", poll, got, want)
		}
	}
}
