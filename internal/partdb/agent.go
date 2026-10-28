package partdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// agentAPI is Part-DB's print agent API (proposed upstream, see docs/usage/labels.md there).
// Like the KiCad API it lives under a language prefix; any language works.
const agentAPI = "/en/agent-api/v1"

// ErrNoAgentAPI means this Part-DB has no print agent API (older or unpatched version).
var ErrNoAgentAPI = errors.New("Part-DB has no print agent API")

type Heartbeat struct {
	Name     string         `json:"name"`
	Version  string         `json:"version"`
	Printers []AgentPrinter `json:"printers"`
}

// AgentPrinter is what Part-DB shows about a printer; it stores the fields without interpreting them.
type AgentPrinter struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Model string `json:"model,omitempty"`
	Media string `json:"media,omitempty"`
}

type Job struct {
	ID      int    `json:"id"`
	Type    string `json:"type"`
	Printer string `json:"printer"`
	Payload struct {
		ProfileID   int    `json:"profile_id"`
		ElementType string `json:"element_type"`
		ElementIDs  string `json:"element_ids"`
	} `json:"payload"`
}

// Heartbeat registers or updates this agent and returns how often to poll.
func (c *Client) Heartbeat(ctx context.Context, uuid string, hb Heartbeat) (pollSeconds int, err error) {
	var res struct {
		PollSeconds int `json:"poll_seconds"`
	}
	status, err := c.agentCall(ctx, http.MethodPost, "/agents/"+url.PathEscape(uuid)+"/heartbeat", hb, &res)
	if status == http.StatusNotFound {
		return 0, ErrNoAgentAPI
	}
	return res.PollSeconds, err
}

// PendingJobs lists this agent's pending jobs, oldest first.
func (c *Client) PendingJobs(ctx context.Context, uuid string) ([]Job, error) {
	var jobs []Job
	_, err := c.agentCall(ctx, http.MethodGet, "/agents/"+url.PathEscape(uuid)+"/jobs", nil, &jobs)
	return jobs, err
}

// ClaimJob claims a pending job; false (and no error) if it is not pending anymore.
func (c *Client) ClaimJob(ctx context.Context, id int) (bool, error) {
	status, err := c.agentCall(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/claim", id), nil, nil)
	if status == http.StatusConflict {
		return false, nil
	}
	return err == nil, err
}

// ReportJob reports the result of a claimed job.
func (c *Client) ReportJob(ctx context.Context, id int, ok bool, message string) error {
	body := struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}{"failed", message}
	if ok {
		body.Status = "done"
	}
	status, err := c.agentCall(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/result", id), body, nil)
	if status == http.StatusConflict || status == http.StatusNotFound {
		return nil // cancelled or removed meanwhile: there is nobody to tell, so don't retry
	}
	return err
}

// agentCall sends JSON (if in != nil) and decodes a 2xx JSON response into out (if not nil).
func (c *Client) agentCall(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.request(ctx, method, agentAPI+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, apiError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(data, out)
}
