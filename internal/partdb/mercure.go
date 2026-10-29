package partdb

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
)

// sseIdleTimeout ends a subscription that received nothing for this long: Mercure sends a keep-alive
// comment every 40 s by default, so silence means the connection died without being closed.
const sseIdleTimeout = 2 * time.Minute

// Mercure is the subscription Part-DB offers in its heartbeat answer.
type Mercure struct {
	Hub   string `json:"hub"`
	Topic string `json:"topic"`
	Token string `json:"token"` // subscriber token for this one topic
}

// Subscribe listens on the hub and calls onEvent once per event. It returns when the stream ends
// (e.g. the token expired), on an error, or when ctx is done.
func (c *Client) Subscribe(ctx context.Context, m Mercure, onEvent func()) error {
	u, err := url.Parse(m.Hub)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid Mercure hub URL %q", m.Hub)
	}
	if u.Scheme != "https" && !c.AllowHTTP && !config.IsLoopback(u.Hostname()) {
		return fmt.Errorf("refusing to subscribe over plain http to %s: use https or allow_http", u.Host)
	}
	idle := c.sseIdle
	if idle == 0 {
		idle = sseIdleTimeout
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errIdle := fmt.Errorf("no data from the Mercure hub for %v", idle)
	watchdog := time.AfterFunc(idle, func() { cancel(errIdle) })
	defer watchdog.Stop()

	q := u.Query()
	q.Set("match", m.Topic)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Accept", "text/event-stream")
	// The stream stays open for as long as the token is valid: no overall timeout.
	resp, err := (&http.Client{Transport: c.HTTP.Transport, CheckRedirect: c.HTTP.CheckRedirect}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Mercure hub: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	sc := bufio.NewScanner(resp.Body)
	hasData := false
	for sc.Scan() {
		watchdog.Reset(idle)
		line := sc.Text()
		switch {
		case line == "":
			if hasData {
				onEvent()
			}
			hasData = false
		case strings.HasPrefix(line, "data:"):
			hasData = true
		}
	}
	if context.Cause(ctx) == errIdle {
		return errIdle
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}
