package partdb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func hubOffer(url string) Mercure {
	return Mercure{Hub: url + "/.well-known/mercure", Topic: "urn:part-db:agent:3:jobs", Token: "sub-token"}
}

func TestSubscribeEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/mercure" || r.URL.Query().Get("match") != "urn:part-db:agent:3:jobs" ||
			r.Header.Get("Authorization") != "Bearer sub-token" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("request: %s %v", r.URL, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ":\n\nid: 1\ndata: {\"job_id\":7}\n\nid: 2\ndata: {\"job_id\":\ndata: 8}\n\n")
	}))
	defer srv.Close()

	events := 0
	err := New(srv.URL, "tcp_x").Subscribe(context.Background(), hubOffer(srv.URL), func() { events++ })
	if err != nil || events != 2 {
		t.Fatalf("events %d, err %v", events, err)
	}
}

func TestSubscribeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := New(srv.URL, "tcp_x").Subscribe(context.Background(), hubOffer(srv.URL), func() { t.Error("event") })
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v", err)
	}
}

func TestSubscribeOutlivesClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "tcp_x")
	c.HTTP.Timeout = 100 * time.Millisecond

	events := 0
	if err := c.Subscribe(context.Background(), hubOffer(srv.URL), func() { events++ }); err != nil || events != 1 {
		t.Fatalf("events %d, err %v", events, err)
	}
}

func TestSubscribeRefusesPlainHTTPHub(t *testing.T) {
	err := New("https://pdb.test", "tcp_x").Subscribe(context.Background(), hubOffer("http://pdb.test"), func() {})
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err %v", err)
	}
}

func TestSubscribeGivesUpOnASilentStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a dead connection: not even the hub's keep-alive comments arrive
	}))
	defer srv.Close()
	c := New(srv.URL, "tcp_x")
	c.sseIdle = 200 * time.Millisecond

	start := time.Now()
	err := c.Subscribe(context.Background(), hubOffer(srv.URL), func() {})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}
