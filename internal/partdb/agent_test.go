package partdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentAPI(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		if r.Header.Get("Authorization") != "Bearer tcp_x" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers: %v", r.Header)
		}
		switch r.URL.Path {
		case "/en/agent-api/v1/agents/u-1/heartbeat":
			_, _ = io.WriteString(w, `{"agent_id": 3, "poll_seconds": 15}`)
		case "/en/agent-api/v1/agents/u-1/jobs":
			_, _ = io.WriteString(w, `[{"id": 7, "type": "print", "printer": "ql", "payload": {"profile_id": 2, "element_type": "part", "element_ids": "1,5-6"}}]`)
		case "/en/agent-api/v1/jobs/7/claim":
			_, _ = io.WriteString(w, `{"id": 7}`)
		case "/en/agent-api/v1/jobs/8/claim":
			w.WriteHeader(http.StatusConflict)
		case "/en/agent-api/v1/jobs/7/result":
			w.WriteHeader(http.StatusNoContent)
		case "/en/agent-api/v1/jobs/8/result":
			w.WriteHeader(http.StatusConflict) // cancelled meanwhile
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, ctx := New(srv.URL, "tcp_x"), context.Background()

	reply, err := c.Heartbeat(ctx, "u-1", Heartbeat{Name: "pc", Version: "1.0", Printers: []AgentPrinter{{ID: "ql", Name: "QL", Media: "dk-22210"}}})
	if err != nil || reply.PollSeconds != 15 || reply.Mercure != nil || reply.Pending != nil {
		t.Fatalf("heartbeat: %+v %v", reply, err)
	}
	var hb map[string]any
	if err := json.Unmarshal([]byte(got[0][len("POST /en/agent-api/v1/agents/u-1/heartbeat "):]), &hb); err != nil || hb["printers"].([]any)[0].(map[string]any)["media"] != "dk-22210" {
		t.Errorf("heartbeat body: %s", got[0])
	}

	jobs, err := c.PendingJobs(ctx, "u-1")
	if err != nil || len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].Printer != "ql" || jobs[0].Payload.ProfileID != 2 ||
		jobs[0].Payload.ElementType != "part" || jobs[0].Payload.ElementIDs != "1,5-6" {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	if ok, err := c.ClaimJob(ctx, 7); !ok || err != nil {
		t.Errorf("claim: %v %v", ok, err)
	}
	if ok, err := c.ClaimJob(ctx, 8); ok || err != nil {
		t.Errorf("claim taken job: %v %v", ok, err)
	}
	if err := c.ReportJob(ctx, 7, false, "cover open"); err != nil {
		t.Errorf("report: %v", err)
	}
	if want := `POST /en/agent-api/v1/jobs/7/result {"status":"failed","message":"cover open"}`; got[len(got)-1] != want {
		t.Errorf("report body:\n got %s\nwant %s", got[len(got)-1], want)
	}

	if err := c.ReportJob(ctx, 8, true, ""); err != nil {
		t.Errorf("a job Part-DB no longer expects a result for is not an error to retry: %v", err)
	}

	if _, err := c.Heartbeat(ctx, "unknown", Heartbeat{}); !errors.Is(err, ErrNoAgentAPI) {
		t.Errorf("Part-DB without the agent API: %v", err)
	}
}

func TestHeartbeatMercureOffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agent_id": 3, "poll_seconds": 30, "pending": 2, "mercure": {"hub": "https://pdb.test/.well-known/mercure", "topic": "urn:part-db:agent:3:jobs", "token": "t"}}`)
	}))
	defer srv.Close()

	reply, err := New(srv.URL, "tcp_x").Heartbeat(context.Background(), "u-1", Heartbeat{})
	want := Mercure{Hub: "https://pdb.test/.well-known/mercure", Topic: "urn:part-db:agent:3:jobs", Token: "t"}
	if err != nil || reply.Mercure == nil || *reply.Mercure != want || reply.Pending == nil || *reply.Pending != 2 {
		t.Fatalf("got %+v %v", reply.Mercure, err)
	}
}
