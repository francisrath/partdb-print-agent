// Package partdb is a minimal Part-DB REST API client.
package partdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
)

const maxPDF = 10 << 20

type Client struct {
	BaseURL   string // e.g. https://partdb.example.org (no /api)
	Token     string // API token; Read-Only is enough for labels
	AllowHTTP bool   // permit plain http to a non-loopback host (token sent unencrypted)
	HTTP      *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" && via[0].URL.Scheme == "https" {
				return errors.New("refusing https → http redirect (would expose the API token)")
			}
			return nil
		},
	}}
}

// FromInstance builds a client for a configured instance.
func FromInstance(in *config.Instance) *Client {
	c := New(in.BaseURL, in.Token)
	c.AllowHTTP = in.AllowHTTP
	return c
}

// request builds an authenticated request, refusing to send the token in clear text.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" && !c.AllowHTTP && !config.IsLoopback(req.URL.Hostname()) {
		return nil, fmt.Errorf("refusing to send the API token over plain %s to %s: use https or allow_http", req.URL.Scheme, req.URL.Host)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return req, nil
}

// GenerateLabels calls POST /api/labels/generate and returns the PDF.
// ids is Part-DB's range syntax ("1,5-10"); elementType is part|part_lot|storelocation or "" for the profile default.
func (c *Client) GenerateLabels(ctx context.Context, profileID int, ids, elementType string) ([]byte, error) {
	body := map[string]any{"profileId": profileID, "elementIds": ids}
	if elementType != "" {
		body["elementType"] = elementType
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := c.request(ctx, http.MethodPost, "/api/labels/generate", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPDF+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.StatusCode, data)
	}
	if len(data) > maxPDF {
		return nil, fmt.Errorf("Part-DB returned more than %d bytes", maxPDF)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, fmt.Errorf("Part-DB did not return a PDF (Content-Type %q)", resp.Header.Get("Content-Type"))
	}
	return data, nil
}

// apiError extracts the message from API Platform's error body (problem+json, JSON-LD or JSON).
func apiError(status int, body []byte) error {
	var e struct {
		Detail      string `json:"detail"`
		Title       string `json:"title"`
		Description string `json:"hydra:description"`
		Message     string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		for _, s := range []string{e.Detail, e.Description, e.Message, e.Title} {
			if s != "" {
				msg = s
				break
			}
		}
	}
	switch status {
	case http.StatusUnauthorized:
		msg = "token rejected (check the API token) " + msg
	case http.StatusForbidden:
		msg = "permission denied " + msg
	}
	return fmt.Errorf("Part-DB: HTTP %d: %s", status, strings.TrimSpace(msg))
}
