// Package agentjobs fetches print jobs from Part-DB's print agent API (architecture §4.1, source 4):
// heartbeat with our printers, list pending jobs, claim one, print it, report the result.
package agentjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/partdb"
)

const (
	defaultPoll  = 30 * time.Second
	minPoll      = 5 * time.Second
	maxPoll      = 10 * time.Minute
	noAPIRetry   = time.Hour       // Part-DB without the agent API: check again later (it may be upgraded)
	printTimeout = 2 * time.Minute // a claimed job finishes even during shutdown, like the HTTP server's prints
)

type Loop struct {
	Client        *partdb.Client
	AgentID       string // stable UUID, so Part-DB keeps our jobs when the token changes
	Name, Version string
	Printers      func() []partdb.AgentPrinter
	// Print prints a claimed print job and returns a short result for Part-DB's job list.
	Print func(ctx context.Context, job partdb.Job) (string, error)
	Log   *slog.Logger

	noAPILogged bool
	unreported  []report // results Part-DB didn't accept yet; retried every round
}

type report struct {
	id      int
	ok      bool
	message string
}

// Run polls until ctx is done.
func (l *Loop) Run(ctx context.Context) error {
	for {
		wait := l.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// tick does one round and returns how long to wait before the next one.
func (l *Loop) tick(ctx context.Context) time.Duration {
	poll, err := l.Client.Heartbeat(ctx, l.AgentID, partdb.Heartbeat{Name: l.Name, Version: l.Version, Printers: l.Printers()})
	switch {
	case errors.Is(err, partdb.ErrNoAgentAPI):
		if !l.noAPILogged {
			l.Log.Info("Part-DB has no print agent API; not fetching print jobs", "retry", noAPIRetry)
			l.noAPILogged = true
		}
		return noAPIRetry
	case err != nil:
		if ctx.Err() == nil {
			l.Log.Warn("print jobs: heartbeat failed", "err", err)
		}
		return defaultPoll
	}
	if l.noAPILogged {
		l.Log.Info("Part-DB print agent API available; fetching print jobs")
		l.noAPILogged = false
	}
	wait := defaultPoll
	if poll > 0 {
		wait = min(max(time.Duration(poll)*time.Second, minPoll), maxPoll)
	}
	l.retryReports(ctx)

	jobs, err := l.Client.PendingJobs(ctx, l.AgentID)
	if err != nil {
		l.Log.Warn("print jobs: listing failed", "err", err)
		return wait
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			break // shutting down: leave the remaining jobs pending
		}
		l.handle(ctx, job)
	}
	return wait
}

func (l *Loop) handle(ctx context.Context, job partdb.Job) {
	// Claim right before printing, so a job is never claimed and left behind.
	ok, err := l.Client.ClaimJob(ctx, job.ID)
	if err != nil || !ok {
		if err != nil {
			l.Log.Warn("print jobs: claim failed", "job", job.ID, "err", err)
		}
		return
	}
	// From here on the job is ours: finish it even if we're shutting down, or a printed label would be
	// reported as failed and might get printed again.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), printTimeout)
	defer cancel()
	var result string
	if job.Type == "print" {
		result, err = l.Print(pctx, job)
	} else {
		err = fmt.Errorf("unsupported job type %q", job.Type)
	}
	if err != nil {
		l.Log.Error("print job failed", "job", job.ID, "err", err)
		result = err.Error()
	} else {
		l.Log.Info("print job done", "job", job.ID, "result", result)
	}
	l.unreported = append(l.unreported, report{job.ID, err == nil, result})
	l.retryReports(pctx) // report even when shutting down, so a printed job doesn't stay "claimed"
}

// retryReports sends the results Part-DB hasn't accepted yet and keeps the ones that fail again.
// ponytail: kept in memory only; after a restart Part-DB shows such a job as "unknown".
func (l *Loop) retryReports(ctx context.Context) {
	var failed []report
	for _, r := range l.unreported {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := l.Client.ReportJob(rctx, r.id, r.ok, r.message)
		cancel()
		if err != nil {
			l.Log.Warn("print jobs: reporting the result failed, will retry", "job", r.id, "err", err)
			failed = append(failed, r)
		}
	}
	l.unreported = failed
}
