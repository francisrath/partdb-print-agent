package partdb

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/francisrath/partdb-print-agent/internal/config"
)

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
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}
