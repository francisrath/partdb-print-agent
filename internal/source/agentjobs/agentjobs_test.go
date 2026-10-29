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

	// Mercure hub at /.well-known/mercure, offered in the heartbeat when push is set
	url        string
	push       chan string // each value is sent as one event; close it to end the stream
	hubStatus  int         // answer subscriptions with this status instead (e.g. 401)
	heartbeats int
	lists      int
	pending    *int // "pending" in the heartbeat answer; nil leaves it out like an older Part-DB
	subscribes int
	subscribed chan struct{} // receives once per subscription that got a stream
}

func (f *fakePartDB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/mercure" {
		f.serveHub(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case f.noAPI:
		http.NotFound(w, r)
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		f.heartbeats++
		poll := f.poll
		if poll == 0 {
			poll = 7
		}
		mercure := ""
		if f.push != nil {
			mercure = fmt.Sprintf(`, "mercure": {"hub": %q, "topic": "urn:part-db:agent:1:jobs", "token": "t"}`, f.url+"/.well-known/mercure")
		}
		if f.pending != nil {
			mercure += fmt.Sprintf(`, "pending": %d`, *f.pending)
		}
		_, _ = fmt.Fprintf(w, `{"agent_id": 1, "poll_seconds": %d%s}`, poll, mercure)
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		f.lists++
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

func (f *fakePartDB) serveHub(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.subscribes++
	status, push := f.hubStatus, f.push
	f.mu.Unlock()
	if r.URL.Query().Get("match") != "urn:part-db:agent:1:jobs" || r.Header.Get("Authorization") != "Bearer t" {
		status = http.StatusBadRequest
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.(http.Flusher).Flush()
	f.subscribed <- struct{}{}
	for {
		select {
		case data, ok := <-push:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakePartDB) count() (heartbeats, subscribes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats, f.subscribes
}

func newLoop(t *testing.T, f *fakePartDB, print func(context.Context, partdb.Job) (string, error)) *Loop {
	srv := httptest.NewServer(f)
	f.url = srv.URL
	f.subscribed = make(chan struct{}, 10)
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

// runLoop runs l until the test ends.
func runLoop(t *testing.T, l *Loop) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestPushWakesLoop(t *testing.T) {
	f := &fakePartDB{jobs: "[]", push: make(chan string)}
	printed := make(chan struct{}, 1)
	l := newLoop(t, f, func(context.Context, partdb.Job) (string, error) { printed <- struct{}{}; return "ok", nil })
	runLoop(t, l)
	waitFor(t, "subscription", f.subscribed)

	f.mu.Lock()
	f.jobs = jobs(j(7, "print"))
	f.mu.Unlock()
	f.push <- `{"job_id":7}`
	waitFor(t, "the pushed job to print (poll is 7 s)", printed)
}

func TestPushReconnectRelists(t *testing.T) {
	f := &fakePartDB{jobs: "[]", push: make(chan string)}
	l := newLoop(t, f, nil)
	l.minSubscription = 50 * time.Millisecond
	runLoop(t, l)
	waitFor(t, "subscription", f.subscribed)
	time.Sleep(100 * time.Millisecond)

	f.mu.Lock()
	close(f.push) // token expired: the hub ends the stream
	f.push = make(chan string)
	f.mu.Unlock()
	waitFor(t, "a new subscription", f.subscribed)
	if hb, _ := f.count(); hb < 2 {
		t.Errorf("%d heartbeats: the loop must fetch a fresh token and re-list jobs after the stream ends", hb)
	}
}

func TestPushHubRejectsFallsBackToPolling(t *testing.T) {
	f := &fakePartDB{jobs: "[]", push: make(chan string), hubStatus: http.StatusUnauthorized}
	runLoop(t, newLoop(t, f, nil))
	time.Sleep(1500 * time.Millisecond)
	if hb, subs := f.count(); hb > 2 || subs > 1 {
		t.Errorf("%d heartbeats, %d subscribe attempts in 1.5 s: a rejecting hub must not cause a tight loop", hb, subs)
	}
}

func TestPushStopsOnCancel(t *testing.T) {
	f := &fakePartDB{jobs: "[]", push: make(chan string)}
	l := newLoop(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()
	waitFor(t, "subscription", f.subscribed)
	cancel()
	waitFor(t, "Run to return", done)
}

func TestPushFailureIsWarnedOnce(t *testing.T) {
	f := &fakePartDB{jobs: "[]", push: make(chan string), hubStatus: http.StatusUnauthorized}
	l := newLoop(t, f, nil)
	var logs strings.Builder
	l.Log = slog.New(slog.NewTextHandler(&logs, nil))
	ended := make(chan bool, 1)
	for range 3 {
		l.subscribe(context.Background(), partdb.Mercure{Hub: f.url + "/.well-known/mercure", Topic: "urn:part-db:agent:1:jobs", Token: "t"}, nil, ended)
		<-ended
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings for a hub that keeps rejecting us, want 1:\n%s", n, logs.String())
	}
}

func TestListsJobsOnlyWhenSomeArePending(t *testing.T) {
	none, two := 0, 2
	f := &fakePartDB{jobs: "[]", pending: &none}
	l := newLoop(t, f, nil)
	l.tick(context.Background())
	if f.lists != 0 {
		t.Errorf("listed jobs although Part-DB reported none pending")
	}
	f.pending = &two
	l.tick(context.Background())
	if f.lists != 1 {
		t.Errorf("%d lists, want 1 when jobs are pending", f.lists)
	}
}

func TestPollWait(t *testing.T) {
	for _, c := range []struct {
		poll int
		live bool
		want time.Duration
	}{
		{30, false, 30 * time.Second},
		{30, true, 60 * time.Second}, // push works: check in less often, still within Part-DB's two-minute online window
		{90, true, 90 * time.Second},
		{1, false, 5 * time.Second},
		{86400, true, 10 * time.Minute},
	} {
		if got := pollWait(c.poll, c.live); got != c.want {
			t.Errorf("pollWait(%d, %v) = %v, want %v", c.poll, c.live, got, c.want)
		}
	}
}
