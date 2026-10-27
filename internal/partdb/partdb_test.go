package partdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/labels/generate" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tcp_good" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"title":"An error occurred","detail":"Invalid API token"}`))
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req["profileId"] != float64(2) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"detail":"Label profile with ID 9 not found."}`))
			return
		}
		if req["elementIds"] != "32,112-125" || req["elementType"] != "part" {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4 fake"))
	}))
	defer srv.Close()
	ctx := context.Background()

	pdf, err := New(srv.URL+"/", "tcp_good").GenerateLabels(ctx, 2, "32,112-125", "part")
	if err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatalf("got %q, %v", pdf, err)
	}
	_, err = New(srv.URL, "tcp_bad").GenerateLabels(ctx, 2, "32", "part")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Invalid API token") {
		t.Errorf("bad token: %v", err)
	}
	_, err = New(srv.URL, "tcp_good").GenerateLabels(ctx, 9, "32", "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing profile: %v", err)
	}
}

func TestTokenAndProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tcp_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/tokens/current":
			w.Write([]byte(`{"name":"agent","user":{"id":2,"name":"fran"},"level":1}`))
		case "/api/label_profiles":
			if r.Header.Get("Accept") != "application/ld+json" {
				w.Write([]byte(`[{"name":"no ids in plain json"}]`))
				return
			}
			switch r.URL.Query().Get("options.supported_element") {
			case "part":
				w.Write([]byte(`{"hydra:member":[{"@id":"/api/label_profiles/2","name":"bag"},{"@id":"/api/label_profiles/1","name":"lid"}]}`))
			case "storelocation":
				w.Write([]byte(`{"hydra:member":[{"@id":"/api/label_profiles/4","name":"drawer"}]}`))
			default:
				w.Write([]byte(`{"hydra:member":[]}`))
			}
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tcp_good")
	tok, err := c.TokenInfo(context.Background())
	if err != nil || tok.Level != 1 || tok.User.Name != "fran" {
		t.Errorf("token: %+v %v", tok, err)
	}
	ps, err := c.Profiles(context.Background())
	if err != nil || len(ps) != 3 || ps[0].ID != 1 || ps[2].Element != "storelocation" {
		t.Errorf("profiles: %+v %v", ps, err)
	}
	if _, err := New(srv.URL, "tcp_bad").TokenInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad token: %v", err)
	}
}

func TestRefusesPlainHTTPAndDowngrade(t *testing.T) {
	ctx := context.Background()
	lan := New("http://partdb.lan", "tcp_x")
	if _, err := lan.GenerateLabels(ctx, 1, "1", ""); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("plain http to a LAN host: %v", err)
	}
	if _, err := lan.TokenInfo(ctx); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("TokenInfo over plain http: %v", err)
	}
	if _, err := lan.Profiles(ctx); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("Profiles over plain http: %v", err)
	}
	// https → http redirect must not follow (it would resend the token in clear text)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+r.URL.Path, http.StatusFound)
	}))
	defer tls.Close()
	c := New(tls.URL, "tcp_x")
	c.HTTP.Transport = tls.Client().Transport
	if _, err := c.GenerateLabels(ctx, 1, "1", ""); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("downgrade redirect: %v", err)
	}
}

func TestRefusesCrossHostRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 127.0.0.1 → localhost: a different host, as a subdomain redirect would be
		http.Redirect(w, r, strings.Replace("http://"+r.Host, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "tcp_x").GenerateLabels(context.Background(), 1, "1", ""); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("cross-host redirect: %v", err)
	}
}

func TestTaggedParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/parts" || q.Get("tags") != "print-label" || len(q["properties[]"]) != 2 {
			http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		if q.Get("page") == "" {
			fmt.Fprint(w, `{"hydra:member":[{"id":32,"lastModified":"2026-10-27T10:00:00+00:00"}],"hydra:view":{"hydra:next":"/api/parts?tags=print-label&properties%5B%5D=id&properties%5B%5D=lastModified&page=2"}}`)
			return
		}
		fmt.Fprint(w, `{"hydra:member":[{"id":7,"lastModified":"2026-10-27T11:00:00+00:00"}],"hydra:view":{}}`)
	}))
	defer srv.Close()
	got, err := New(srv.URL, "tcp_x").TaggedParts(context.Background(), "print-label")
	if err != nil || len(got) != 2 || got[0].ID != 32 || got[1].ID != 7 || got[1].LastModified == "" {
		t.Errorf("got %+v, %v", got, err)
	}
}
