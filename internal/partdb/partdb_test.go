package partdb

import (
	"context"
	"encoding/json"
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

func TestRefusesPlainHTTPAndDowngrade(t *testing.T) {
	ctx := context.Background()
	if _, err := New("http://partdb.lan", "tcp_x").GenerateLabels(ctx, 1, "1", ""); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("plain http to a LAN host: %v", err)
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
