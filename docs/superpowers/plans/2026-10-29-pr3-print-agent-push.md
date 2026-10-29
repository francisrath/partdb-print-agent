# PR 3 – Instant print-job delivery via Mercure – Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A print job created in Part-DB reaches a connected agent within about a second instead of at the next poll, and FrankenPHP's built-in Mercure hub starts again.

**Architecture:** Part-DB publishes a private Mercure update for the agent whenever it creates or resends a job, and its heartbeat answer offers the agent `{hub, topic, token}`. The agent keeps one SSE subscription per instance and treats every event as "look for jobs now". Polling stays exactly as in PR 2 and is the fallback: no hub, a broken hub or an expired token only means the agent waits for the next poll.

**Tech Stack:** Part-DB: Symfony 7.4, `symfony/mercure-bundle` 0.6.1 / `symfony/mercure` 0.9.0 (checked live 2026-10-29; both default to the Mercure 1.0 protocol), PHPUnit. Hub: Mercure 1.1.5 (bundled in FrankenPHP 1.13.4). Agent: Go stdlib only (`bufio` SSE reader).

**Spec:** `docs/superpowers/specs/2026-10-28-partdb-agent-prs-design.md` § "PR 3 – instant delivery via Mercure" and § "`pdb-print` PR".

## Verified before planning (spec's open risk)

- `symfony/mercure` 0.9 `LcobucciFactory` mints Mercure 1.x tokens (`typ: at+jwt`, `authorization_details`) and requires `iss`, `aud`, `sub`, `client_id`. The bundle's `jwt.claims` supplies `iss`/`sub`/`client_id`; `aud` defaults to the hub's `public_url` (plus `url` for publisher tokens). So Part-DB does **not** need to sign tokens itself.
- Mercure 1.x (UPGRADE.md, "JWT keys move into `issuer` blocks"): the top-level `publisher_jwt`/`subscriber_jwt` directives that Part-DB's `.docker/frankenphp/Caddyfile` uses are "a configuration error" without `protocol_version_compatibility`. The fix is `issuer <iss> { publisher { jwt … } subscriber { jwt … } }`, as in FrankenPHP's own Caddyfile (`issuer {$MERCURE_TRUSTED_ISSUERS:https://localhost}`). `transport_url` still works in official builds (deprecated tag), so it stays.
- The hub closes a subscription at token expiry; reconnecting needs a fresh token. Subscribers send the token in the `Authorization` header (no query parameter in 1.x) and subscribe with `?match=<topic>`.

## Global Constraints

- Part-DB code is AGPL; nothing from it is copied into pdb-print (MIT). Part-DB files get the AGPL header (`scratchpad/hdr.txt`).
- Every user-facing Part-DB string is a translation key in `translations/messages.en.xlf` (this plan adds none).
- PHPStan level 5, `lint:twig`, `lint:yaml`, `doctrine:schema:validate` must pass. No migration in this PR.
- Push is **off unless `MERCURE_PUBLIC_URL` is set**. With it unset, Part-DB behaves exactly like PR 2 (no publish attempt, no `mercure` key in the heartbeat).
- A failed publish never fails job creation: log a warning and rely on polling.
- Tests never print and never talk to a real hub: Part-DB uses `MockHub`, pdb-print a fake hub on 127.0.0.1.
- Local only, no push. Conventional commits ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never commit `config/reference.php`.
- Branches: Part-DB `feat/print-agent-mercure` from `feat/print-agent-jobs` (54f44599); pdb-print continues on `feat/agent-jobs` (one agent PR for PR 2 + 3, per spec).

## Rulings made while planning

- **Topic is `urn:part-db:agent:<agent id>:jobs`, not `<base>/agents/<id>/jobs`.** The base URL depends on the request host, so a job created via `partdb.example.org` and a heartbeat via a LAN IP would get different topics and the event would never arrive. Cost if wrong: a one-line rename.
- **Enabled iff `MERCURE_PUBLIC_URL` is non-empty.** `MERCURE_URL` may legitimately be empty (the bundle then publishes through FrankenPHP's built-in hub), and the agent needs the public URL anyway. Cost if wrong: one env check.
- **Subscriber token lives 1 hour** and carries `sub` = agent UUID. The agent re-heartbeats every poll anyway, so a dropped subscription picks up a new token immediately.
- **Agent keeps polling at the server's interval while subscribed.** Heartbeats also keep the agent "online" (120 s), so polling can't be stretched without changing PR 2's online rule.
- **Caddyfile:** only the JWT directives move into an `issuer {$MERCURE_TRUSTED_ISSUERS:https://localhost}` block; everything else stays, to keep the diff reviewable.

## Review Focus

1. Hub down or misconfigured (wrong secret, wrong issuer) → job creation still succeeds and the agent still prints at the next poll; no tight reconnect loop. Tests: `testPublishFailureDoesNotBreakJobCreation`, `TestPushHubRejectsFallsBackToPolling`.
2. Token expiry / hub closes the stream → agent re-heartbeats, re-lists pending jobs right away, resubscribes. Test: `TestPushReconnectRelists`.
3. Another user's agent must not get a token for, or events from, someone else's topic: the token grants exactly one topic and the update is private. Test: `testHeartbeatOffersMercureWhenEnabled` decodes the token.
4. The SSE request must not use the 60 s timeout of the Part-DB HTTP client (it would drop the stream every minute). Test: `TestSubscribeOutlivesClientTimeout`.
5. Shutdown while subscribed → `Run` returns promptly. Test: existing shutdown test plus `TestPushStopsOnCancel`.

---

### Task 1: Mercure bundle, config and the push service (Part-DB)

**Files:**
- Modify: `composer.json`, `composer.lock`, `symfony.lock`, `config/bundles.php` (via `composer require symfony/mercure-bundle:^0.6.1` in the dev container; drop any recipe file this plan doesn't list)
- Create: `config/packages/mercure.yaml`
- Modify: `.env` (commented defaults, next to the OAuth block style)
- Create: `src/Services/LabelSystem/AgentJobPusher.php`
- Modify: `src/Services/LabelSystem/AgentJobCreator.php` (`save()` calls the pusher after `flush()`)
- Test: `tests/Services/LabelSystem/AgentJobPusherTest.php`

**Interfaces:**
- Produces: `AgentJobPusher::isEnabled(): bool`, `AgentJobPusher::topic(Agent $agent): string` (`urn:part-db:agent:<id>:jobs`), `AgentJobPusher::notify(AgentJob $job): void` (private `Update`, data `{"job_id":<id>}`; catches `\Throwable`, logs warning), `AgentJobPusher::subscription(Agent $agent): ?array` returning `['hub' => <public url>, 'topic' => …, 'token' => …]` or `null` when disabled.

`config/packages/mercure.yaml`:

```yaml
mercure:
    hubs:
        default:
            url: '%env(default::MERCURE_URL)%'
            public_url: '%env(default::MERCURE_PUBLIC_URL)%'
            jwt:
                secret: '%env(default::MERCURE_JWT_SECRET)%'
                claims:
                    iss: '%env(MERCURE_JWT_ISSUER)%'
                    sub: 'part-db'
                    client_id: 'part-db'
```

`.env` gets `MERCURE_URL=`, `MERCURE_PUBLIC_URL=`, `MERCURE_JWT_SECRET=`, `MERCURE_JWT_ISSUER=https://localhost` with a comment that push is off while `MERCURE_PUBLIC_URL` is empty. If the container doesn't compile with empty values, ledger a ruling and use the smallest fix (e.g. a placeholder default).

- [ ] **Step 1: Branch and install** — `git switch -c feat/print-agent-mercure feat/print-agent-jobs`; check the newest `symfony/mercure-bundle` on Packagist again, then `composer require` it in the dev container. Expected: `cache:clear` succeeds in dev and test with the env vars empty.
- [ ] **Step 2: Write the failing tests** in `AgentJobPusherTest` (KernelTestCase, replace `HubInterface` in the container with a `MockHub` that records updates; set `MERCURE_PUBLIC_URL` through the service's constructor argument, not the process env):
  - `testDisabledWithoutPublicUrl`: `isEnabled()` false, `subscription()` null, `notify()` publishes nothing.
  - `testNotifyPublishesPrivateUpdate`: one update, `getTopics() === ['urn:part-db:agent:<id>:jobs']`, `isPrivate()` true, data decodes to `['job_id' => <id>]`.
  - `testPublishFailureDoesNotBreakJobCreation`: `MockHub` publisher throws; `AgentJobCreator::create()` still returns a persisted job.
  - `testCreateAndResendNotify`: `create()` and `resend()` each publish once.
- [ ] **Step 3: Run** `dct php bin/phpunit tests/Services/LabelSystem/AgentJobPusherTest.php`. Expected: FAIL, class not found.
- [ ] **Step 4: Implement** `AgentJobPusher` (constructor: `HubInterface $hub`, `#[Autowire('%env(default::MERCURE_PUBLIC_URL)%')] ?string $publicUrl`, `LoggerInterface $logger`); token via `$hub->getFactory()->create([new Grant([Grant::ACTION_SUBSCRIBE], [$topic])], ['sub' => $agent->getUuid(), 'exp' => new \DateTimeImmutable('+1 hour')])` (check the claim value type `LcobucciFactory` wants for `exp`). Wire `notify()` into `AgentJobCreator::save()`.
- [ ] **Step 5: Run** the test file plus `tests/Controller/LabelControllerTest.php` and `tests/Controller/AgentApiControllerTest.php`. Expected: PASS (PR 2 tests unchanged, since push is off there).
- [ ] **Step 6: Commit** `feat(agents): publish new print jobs to Mercure`.

### Task 2: Heartbeat offers the subscription (Part-DB)

**Files:**
- Modify: `src/Controller/AgentApiController.php` (`heartbeat()` adds `'mercure' => $pusher->subscription($agent)` only when non-null)
- Test: `tests/Controller/AgentApiControllerTest.php`

**Interfaces:**
- Consumes: `AgentJobPusher::subscription(Agent): ?array` from Task 1.
- Produces: heartbeat JSON `{"agent_id", "poll_seconds", "mercure"?: {"hub", "topic", "token"}}` (pdb-print Task 4 reads it).

- [ ] **Step 1: Write the failing tests:**
  - `testHeartbeatOmitsMercureWhenDisabled`: response has no `mercure` key.
  - `testHeartbeatOffersMercureWhenEnabled`: with the pusher service replaced by one built with public URL `http://127.0.0.1:3000/.well-known/mercure` and a `MockHub` whose factory is a real `LcobucciFactory` (secret `test-secret-at-least-256-bits-long!!`), `mercure.hub` equals that URL, `mercure.topic` equals `urn:part-db:agent:<id>:jobs`, and the token's payload (base64url-decode the middle part) has `sub` = the agent UUID, an `exp` within 3600 ± 60 s, and exactly one `authorization_details` entry with `actions == ['subscribe']` and the one topic.
- [ ] **Step 2: Run** them. Expected: the enabled test FAILs on the missing key.
- [ ] **Step 3: Implement** the heartbeat change.
- [ ] **Step 4: Run** `dct php bin/phpunit tests/Controller/AgentApiControllerTest.php`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(agents): offer a Mercure subscription in the heartbeat`.

### Task 3: FrankenPHP Caddyfile and docs (Part-DB)

**Files:**
- Modify: `.docker/frankenphp/Caddyfile` (JWT directives → `issuer {$MERCURE_TRUSTED_ISSUERS:https://localhost} { publisher { jwt … } subscriber { jwt … } }`)
- Modify: `docs/configuration.md` (new "### Print agent push (Mercure) settings (all env only)" after the OAuth2 section: the four `MERCURE_*` variables; on FrankenPHP set `MERCURE_PUBLISHER_JWT_KEY` and `MERCURE_SUBSCRIBER_JWT_KEY` to the same value as `MERCURE_JWT_SECRET`, and `MERCURE_TRUSTED_ISSUERS` to `MERCURE_JWT_ISSUER`; leave `MERCURE_URL` empty there; on the Apache image run the `dunglas/mercure` image as a sidecar and set `MERCURE_URL` to its internal URL)
- Modify: `docs/usage/labels.md` ("Print jobs" section: one paragraph on instant delivery, polling fallback)

- [ ] **Step 1: Prove the bug.** Pull `dunglas/frankenphp:1-php8.4-bookworm`, print `frankenphp version` (must bundle Mercure ≥ 1.0), and run `frankenphp adapt --config /etc/caddy/Caddyfile` with the **current** Caddyfile mounted and the `MERCURE_*_JWT_KEY` vars set. Expected: an error about `publisher_jwt` / compatibility mode. If it adapts cleanly, stop and ledger it: the "fix" premise is wrong.
- [ ] **Step 2: Change the Caddyfile.** Same command. Expected: adapts without error; the JSON shows an `issuers` entry.
- [ ] **Step 3: Docs** as listed above. `dct php bin/console lint:yaml config` passes.
- [ ] **Step 4: Commit** `fix(docker): Mercure 1.x issuer block in the FrankenPHP Caddyfile` and `docs(labels): instant print job delivery via Mercure` (two commits).

### Task 4: Heartbeat reply and SSE subscriber (pdb-print)

**Files:**
- Modify: `internal/partdb/agent.go`
- Create: `internal/partdb/mercure.go`
- Test: `internal/partdb/agent_test.go`, `internal/partdb/mercure_test.go`

**Interfaces:**
- Produces:
  - `type Mercure struct { Hub, Topic, Token string }` (json `hub`, `topic`, `token`)
  - `type HeartbeatReply struct { PollSeconds int; Mercure *Mercure }`; `Heartbeat(ctx, uuid, hb) (HeartbeatReply, error)` (replaces the `pollSeconds int` return; update the one caller in `agentjobs`).
  - `func (c *Client) Subscribe(ctx context.Context, m Mercure, onEvent func()) error`: `GET <Hub>?match=<QueryEscape(Topic)>`, headers `Authorization: Bearer <Token>`, `Accept: text/event-stream`; non-2xx → error with the status; calls `onEvent` once per SSE event that has a `data:` line; returns when the stream ends or ctx is done. Uses `&http.Client{Transport: c.HTTP.Transport}` (no timeout). Hub must be `http`/`https`, else error.

- [ ] **Step 1: Write the failing tests:**
  - `TestHeartbeatMercureOffer`: fake answer with `mercure` → `reply.Mercure` equals it; without → `nil`.
  - `TestSubscribeEvents`: fake hub checks `match` query and bearer header, writes two events (one split over two `data:` lines) and a `:` comment, then closes → `onEvent` called 2 times, returns `nil`.
  - `TestSubscribeRejected`: hub answers 401 → error containing `401`, `onEvent` never called.
  - `TestSubscribeOutlivesClientTimeout`: `Client.HTTP.Timeout = 100ms`; hub sends one event after 300 ms → `onEvent` called once.
- [ ] **Step 2: Run** `go test ./internal/partdb/`. Expected: FAIL (undefined: `Mercure`, `Subscribe`).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/partdb/ ./internal/source/agentjobs/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(partdb): Mercure subscription offer and SSE subscriber`.

### Task 5: Loop wakes on push (pdb-print)

**Files:**
- Modify: `internal/source/agentjobs/agentjobs.go`
- Test: `internal/source/agentjobs/agentjobs_test.go` (fake Part-DB gains an optional fake hub at `/.well-known/mercure` on the same server and a way to push an event / close the stream / answer 401)
- Modify: `README.md` (one sentence: jobs arrive instantly when Part-DB offers Mercure, polling otherwise)

**Interfaces:**
- Consumes: `HeartbeatReply`, `Client.Subscribe` from Task 4.

Behaviour: `tick` returns the wait and the offer. `Run` starts one subscription goroutine when an offer arrives and none is running. An event wakes `Run` immediately (`tick` runs: heartbeat, retry reports, list, print). When the subscription ends, `Run` ticks immediately and resubscribes if it lasted at least `minPoll` (5 s); a shorter-lived subscription (hub rejects, crashes) is logged once per failure and retried only at the next regular poll. Wake signals go through a 1-buffered channel, so bursts collapse into one round.

- [ ] **Step 1: Write the failing tests** (poll 7 s in all of them, so anything within 2 s proves push):
  - `TestPushWakesLoop`: start `Run`; after the subscription is open, add a job and push an event → printed within 2 s.
  - `TestPushReconnectRelists`: subscription open for ≥ 5 s — use a `minPoll` override field on `Loop` for tests (`minPoll time.Duration`, default constant) set to 100 ms; hub closes the stream → a second heartbeat and a second subscription within 2 s.
  - `TestPushHubRejectsFallsBackToPolling`: hub answers 401 → over 1.5 s at most 2 heartbeats and at most 1 subscribe attempt.
  - `TestPushStopsOnCancel`: cancel ctx while subscribed → `Run` returns within 1 s.
- [ ] **Step 2: Run** `go test ./internal/source/agentjobs/ -run Push`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./... && go vet ./... && staticcheck ./...` (staticcheck at the latest release, checked live). Expected: all pass.
- [ ] **Step 5: Commit** `feat(agentjobs): wake on Mercure events, keep polling as fallback`.

### Task 6: End-to-end check (no commit unless something breaks)

- [ ] Run `dunglas/mercure:v1.1.5` on port 3000 with an `issuer https://localhost` block (publisher and subscriber key = a test secret, `MERCURE_EXTRA_DIRECTIVES` empty).
- [ ] Start Part-DB's dev server with `MERCURE_URL=http://host.docker.internal:3000/.well-known/mercure`, `MERCURE_PUBLIC_URL=http://localhost:3000/.well-known/mercure`, the same secret, issuer `https://localhost`.
- [ ] Start the fake printer sink and `pdb-print` with `scratchpad/e2e/config.yaml`; the server's poll interval is 30 s.
- [ ] Click the menu print icon (CDP script). Expected: the sink receives the job in under 3 s and the agent log shows the subscription. Then stop the hub: the next job still prints, within one poll interval, and job creation shows no error.
- [ ] Stop the hub, agent, sink. Revert any env change to the dev env.

### Final

Whole-branch review of both repos (fresh reviewer, most capable model), fix pass, then the summary for the user, including the rulings above.
