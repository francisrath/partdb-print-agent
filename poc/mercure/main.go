// Command mercure-poc is a throwaway proof of concept for agent push over Mercure
// (Part-DB-server#1625, proposal step 3). It plays both sides: "Part-DB" minting tokens and
// publishing jobs, and the agent subscribing and printing. Not part of the product.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/francisrath/partdb-print-agent/internal/config"
	"github.com/francisrath/partdb-print-agent/internal/partdb"
	"github.com/francisrath/partdb-print-agent/internal/printing"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mercure-poc token|publish|subscribe|agent [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "token":
		err = tokenCmd(os.Args[2:])
	case "publish":
		err = publishCmd(ctx, os.Args[2:])
	case "subscribe":
		err = subscribeCmd(ctx, os.Args[2:], nil)
	case "agent":
		err = agentCmd(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// mint builds a Mercure v1 access token (RFC 9068 at+jwt, HS256) granting action on topics.
func mint(key, iss, aud, action string, topics []string, ttl time.Duration) string {
	var grants []map[string]string
	for _, t := range topics {
		g := map[string]string{"match": t}
		if strings.ContainsAny(t, ":*") && strings.Count(t, ":") > 1 || strings.Contains(t, "/*") {
			g["match_type"] = "urlpattern" // e.g. https://partdb.example.org/agents/* (scheme ":" alone is exact)
		}
		grants = append(grants, g)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": iss, "aud": aud, "sub": iss + "/agents/poc", "client_id": iss,
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
		"authorization_details": []map[string]any{{
			"type": "https://mercure.rocks/authorization-detail", "actions": []string{action}, "topics": grants,
		}},
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	unsigned := enc(map[string]string{"alg": "HS256", "typ": "at+jwt"}) + "." + enc(claims)
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func tokenCmd(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	key := fs.String("key", "", "HMAC key")
	iss := fs.String("iss", "https://partdb.example.org", "issuer")
	aud := fs.String("aud", "http://127.0.0.1:3901/.well-known/mercure", "audience (hub resource identifier)")
	action := fs.String("action", "subscribe", "publish|subscribe")
	topic := fs.String("topic", "", "topic (comma-separated)")
	ttl := fs.Duration("ttl", time.Hour, "lifetime")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Println(mint(*key, *iss, *aud, *action, strings.Split(*topic, ","), *ttl))
	return nil
}

func publishCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	hub := fs.String("hub", "http://127.0.0.1:3901/.well-known/mercure", "hub URL")
	token := fs.String("token", "", "publisher token")
	topic := fs.String("topic", "", "topic")
	data := fs.String("data", "", "payload (a JSON object; sent_at is added)")
	private := fs.Bool("private", true, "private update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload := map[string]any{}
	if *data != "" {
		if err := json.Unmarshal([]byte(*data), &payload); err != nil {
			return err
		}
	}
	payload["sent_at"] = time.Now().UnixMicro()
	b, _ := json.Marshal(payload)
	form := url.Values{"topic": {*topic}, "data": {string(b)}}
	if *private {
		form.Set("private", "on")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *hub, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+*token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("publish: HTTP %d %s\n", resp.StatusCode, bytes.TrimSpace(body))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("publish failed")
	}
	return nil
}

// subscribeCmd prints every event (and its delivery latency); onEvent, if set, handles it.
func subscribeCmd(ctx context.Context, args []string, onEvent func(ctx context.Context, data []byte)) error {
	fs := flag.NewFlagSet("subscribe", flag.ContinueOnError)
	hub := fs.String("hub", "http://127.0.0.1:3901/.well-known/mercure", "hub URL")
	token := fs.String("token", "", "subscriber token (empty = anonymous)")
	topic := fs.String("topic", "", "topic to subscribe to")
	timeout := fs.Duration("timeout", 0, "stop after this long (0 = until interrupted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	// Mercure 1.x: exact matcher is "match" (0.x used "topic")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *hub+"?match="+url.QueryEscape(*topic), nil)
	req.Header.Set("Accept", "text/event-stream")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fmt.Printf("subscribe %s: HTTP %d\n", *topic, resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("subscribe refused: %s %s", resp.Header.Get("WWW-Authenticate"), bytes.TrimSpace(b))
	}
	sc := bufio.NewScanner(resp.Body)
	var data []byte
	events := 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
		case line == "" && len(data) > 0:
			events++
			var p struct {
				SentAt int64 `json:"sent_at"`
			}
			_ = json.Unmarshal(data, &p)
			fmt.Printf("event %d: %s (latency %v)\n", events, data, time.Since(time.UnixMicro(p.SentAt)).Round(time.Microsecond*100))
			if onEvent != nil {
				onEvent(ctx, data)
			}
			data = nil
		}
	}
	if ctx.Err() != nil {
		fmt.Printf("subscribe %s: done, %d event(s)\n", *topic, events)
		return nil
	}
	return sc.Err()
}

// agentCmd subscribes and prints each job {"profile":2,"ids":"32"} through the real agent code.
func agentCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "pdb-print config (printer + instance)")
	printerID := fs.String("printer", "", "printer id")
	split := 0
	for i, a := range args {
		if a == "--" {
			split = i
		}
	}
	if err := fs.Parse(args[:split]); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath, true)
	if err != nil {
		return err
	}
	p, err := cfg.Printer(*printerID)
	if err != nil {
		return err
	}
	in, err := cfg.Instance("")
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc := &printing.Service{Log: log}
	defer svc.Close()
	return subscribeCmd(ctx, args[split+1:], func(ctx context.Context, data []byte) {
		var job struct {
			Profile int    `json:"profile"`
			IDs     string `json:"ids"`
			SentAt  int64  `json:"sent_at"`
		}
		if err := json.Unmarshal(data, &job); err != nil || job.Profile == 0 {
			log.Error("bad job", "data", string(data))
			return
		}
		pdf, err := partdb.FromInstance(in).GenerateLabels(ctx, job.Profile, job.IDs, "")
		if err == nil {
			_, err = svc.Print(ctx, p, "", printing.PDF(pdf))
		}
		log.Info("job done", "err", err, "publish→done", time.Since(time.UnixMicro(job.SentAt)).Round(time.Millisecond))
	})
}
