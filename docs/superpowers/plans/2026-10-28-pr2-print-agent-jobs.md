# PR 2 – server-side print jobs – Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Part-DB stores print jobs; the user's agents fetch them with a read-only token, so printing works in one click from any device, phone included.

**Architecture:** Two plain Doctrine entities (`Agent`, `AgentJob`), a small JSON controller for agents (token auth, like the KiCad API), a service that creates jobs, and UI hooks in the places PR 1 touched (dialog, label menu, parts table) plus a "Print agents" page. `pdb-print` gets an agent loop that heartbeats, claims jobs and prints them through its existing path.

**Tech Stack:** Part-DB (Symfony 7, Doctrine, Twig, PHPUnit) in Docker; `pdb-print` (Go).

**Spec:** `docs/superpowers/specs/2026-10-28-partdb-agent-prs-design.md` (section "PR 2" and "pdb-print PR").

## Global Constraints

- Part-DB: branch `feat/print-agent-jobs` from `feat/print-agent-button`; agent: branch `feat/agent-jobs` from `feat/partdb-print-button`. **No push, no fork, no PR.**
- `dct` = scratchpad wrapper running a command in the dev container against the test DB (`var/db/test.db`, `APP_ENV=test`); `dc` = `docker compose -f compose.dev.yaml run --rm -T partdb`.
- AGENTS.md rules from PR 1 apply (translations + `translation:touch-catalog` via `dc`, phpstan, lint:twig, `doctrine:schema:validate --skip-sync`, migrations for MySQL, PostgreSQL and SQLite).
- Entities do **not** extend `AbstractDBElement` (like `ApiToken`): the event log would throw for unknown log target types and would log every heartbeat.
- An agent authenticates with any API token of a user who has `@labels.create_labels` (its `apiTokenRole` is READ_ONLY). An agent only sees its own jobs; only its owner can send it jobs.
- Never re-queue a job automatically. `claimed` older than 10 min shows as "unknown". Finished jobs (`done`, `failed`, `cancelled`) older than 30 days are deleted whenever a job is created.
- Job payload JSON (exact keys): `{"profile_id": int, "element_type": "part|part_lot|storelocation", "element_ids": "1,2,5-10"}`; `element_ids` normalised like PR 1 (no spaces, no empty entries).
- Agent API base path: `/agent-api/v1`. JSON in and out. Errors: 400 bad input, 403 no permission, 404 unknown agent/job (also for other users' jobs), 409 job not pending.
- Agent never-print rule for `pdb-print` tests: fake Part-DB and fake printer on 127.0.0.1 only.

## Review Focus

1. Another user's token must not see, claim or report another user's jobs (404, not 403, so ids don't leak). → Task 2 test.
2. Two agents/processes claiming the same job: exactly one wins (conditional UPDATE), the other gets 409. → Task 2 test (claim twice).
3. A heartbeat with a garbage `printers` value (not a list, huge payload) is rejected with 400; names/ids are length-limited (255). → Task 2 test.
4. A profile whose default printer no agent of the current user reports falls back to PR 1 behaviour (local agent / dialog), never a dead button. → Task 4 test.
5. Agent offline (last seen > 2 min): Part-DB still accepts the job but the flash says the agent is offline and the job waits. → Task 3 test.

---

### Task 1: Entities, migration, profile default printer

**Files:** Create `src/Entity/AgentSystem/Agent.php`, `src/Entity/AgentSystem/AgentJob.php`, `src/Entity/AgentSystem/AgentJobStatus.php` (string enum `pending|claimed|done|failed|cancelled`), `src/Repository/AgentJobRepository.php`, `migrations/Version2026102812xxxx.php`; modify `src/Entity/LabelSystem/LabelProfile.php`. Test: `tests/Entity/AgentSystem/AgentJobTest.php`.

**Interfaces (produces):**
- `Agent`: table `agents`; `id` int, `uuid` string(36), `name` string(255), `owner` ManyToOne `User` (onDelete CASCADE), `version` string(64), `printers` JSON (list), `lastSeenAt` ?DateTimeImmutable, `createdAt` DateTimeImmutable; unique (`owner`, `uuid`). Methods: getters/setters, `isOnline(\DateTimeImmutable $now): bool` (last seen ≤ 120 s), `hasPrinter(string $id): bool`, `getPrinterName(string $id): string` (falls back to id).
- `AgentJob`: table `agent_jobs`; `id`, `agent` ManyToOne `Agent` (CASCADE), `printer` string(255), `type` string(32) default `print`, `payload` JSON, `status` `AgentJobStatus`, `message` text default '', `createdBy` ManyToOne `User` (SET NULL, nullable), `createdAt`, `updatedAt`. Methods: `isStale(\DateTimeImmutable $now): bool` (claimed and updatedAt older than 600 s), `getLabelCount(RangeParser)` not needed – the UI shows `element_ids`.
- `AgentJobRepository::claim(AgentJob $job): bool` – `UPDATE agent_jobs SET status='claimed', updated_at=:now WHERE id=:id AND status='pending'`, returns affected rows === 1. `pruneFinished(\DateTimeImmutable $before): int`.
- `LabelProfile::$default_printer` string(255) default `''` + getter/setter, in groups `extended, full, import, label_profile:read`.

- [ ] **Step 1:** branches (`git switch -c` in both repos).
- [ ] **Step 2:** Test `testStaleAndOnline`: Agent with lastSeenAt now-60s → online, now-200s → offline; job claimed with updatedAt now-601s → stale, pending → never stale; `hasPrinter('ql810')` true for printers `[{id:'ql810',name:'QL'}]`, `getPrinterName('x')` = 'x'. Run → FAIL (classes missing).
- [ ] **Step 3:** Write entities/enum/repository. Run test → PASS.
- [ ] **Step 4:** Migration: generate per platform with `doctrine:migrations:diff` against throwaway MariaDB and PostgreSQL containers (and SQLite), merge the three into one `AbstractMultiPlatformMigration` with up/down for each. `dc php bin/console doctrine:migrations:migrate -n` (dev), same with `dct`; `dct php bin/console doctrine:schema:validate` → OK both mapping and DB.
- [ ] **Step 5:** Commit `feat(agents): agent and agent job entities`.

### Task 2: Agent API

**Files:** Create `src/Controller/AgentApiController.php`. Test: `tests/Controller/AgentApiControllerTest.php`.

**Interfaces:**
- `POST /agent-api/v1/agents/{uuid}/heartbeat` body `{name, version, printers: [{id, name, ...}]}` → 200 `{"agent_id": int, "poll_seconds": 30}`. Creates or updates the `Agent` for (token user, uuid); stores `printers` as given (each must be an object with string `id` ≤ 255 and optional string `name`; ≤ 50 printers; else 400). `uuid` must match `^[0-9a-fA-F-]{8,36}$`.
- `GET /agent-api/v1/agents/{uuid}/jobs` → 200 `[{id, type, printer, payload, created_at}]` pending jobs of that agent, oldest first, max 20. Also updates `lastSeenAt`.
- `POST /agent-api/v1/jobs/{id}/claim` → 200 job JSON / 409 / 404.
- `POST /agent-api/v1/jobs/{id}/result` body `{status: "done"|"failed", message?: string ≤ 2000}` → 204; only from `claimed`, else 409.
- All: `denyAccessUnlessGranted('@labels.create_labels')`; agent/job must belong to the token's user (else 404).

- [ ] **Step 1:** Tests (WebTestCase, header `HTTP_AUTHORIZATION: Bearer tcp_readonly` = admin's read-only fixture token; a second user's token via a fixture user – create one in the test with a fresh `ApiToken` if no fixture fits): heartbeat creates then updates (same agent_id); jobs list returns a pending job created in the test; claim → 200 then 409; result done → 204, second result → 409; other user's token → 404 on jobs/claim/result; bad printers (`"x"`, 51 printers, id of 300 chars) → 400; no token → 401.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** Run → PASS. **Step 5:** Commit `feat(agents): JSON API for print agents`.

### Task 3: Creating jobs from the UI

**Files:** Create `src/Services/LabelSystem/AgentJobCreator.php`, modify `src/Controller/LabelController.php` (new action). Test: `tests/Controller/LabelControllerTest.php`.

**Interfaces:**
- `AgentJobCreator::printersFor(User $user): array` → list of `['agent' => Agent, 'id' => string, 'name' => string]`, user's agents, most recently seen first.
- `AgentJobCreator::defaultPrinterFor(User $user, LabelProfile $profile): ?array` → the first entry of `printersFor` whose id equals `$profile->getDefaultPrinter()` (non-empty), else null.
- `AgentJobCreator::create(User $user, Agent $agent, string $printer, LabelProfile $profile, string $elementType, string $ids): AgentJob` – checks ownership and `hasPrinter`, normalises ids, prunes finished jobs older than 30 days, persists, flushes.
- Route `label_print` `POST /label/print` with fields `profile`, `target_type`, `target_id`, `agent`, `printer`, `_token` (CSRF id `label_print`), `_redirect` (local path only). Flash success `label_generator.print.sent` ("Sent %count% label(s) to %printer%" – count via `RangeParser`) or, when the agent is offline, `label_generator.print.sent_offline` ("… %printer% is offline; the job waits until it is back"). Redirect to `_redirect` or the dialog.

- [ ] **Step 1:** Tests: POST with a fixture agent (created in the test for admin) → 302, one pending job with exact payload; agent of another user → 404; bad CSRF → 4xx and no job; offline agent → flash contains "offline". Run → FAIL.
- [ ] **Step 2:** Implement. Run → PASS. Commit `feat(labels): send print jobs to an agent`.

### Task 4: Print buttons use agent printers

**Files:** Modify `templates/label_system/dialog.html.twig`, `templates/label_system/dropdown_macro.html.twig`, `templates/components/datatables.macro.html.twig`, `src/Services/Parts/PartsTableActionHandler.php`, add a Twig function `agent_printers()` / `agent_default_printer(profile)` (in `src/Twig/` next to an existing extension, backed by `AgentJobCreator`). Test: `tests/Controller/LabelControllerTest.php`, `tests/Services/Parts/PartsTableActionHandlerTest.php` if one exists, else in LabelControllerTest via the table action route.

- Dialog: when the user has agent printers, a form POSTing to `label_print` with a split button: main = default printer of the profile (or first printer), `▾` lists all printers (each a submit button carrying `agent`/`printer`) and, when the browser is connected (PR 1 controller), an item "This computer". Without agent printers: PR 1 button unchanged.
- Label menu: when `agent_default_printer(profile)` exists, the icon is a tiny POST form to `label_print` (visible without JS); else the PR 1 icon.
- Parts table: new action `print_label` (and `print_label_lot`) with the profile select; handler creates the job with the profile's default printer and redirects back with the flash; without one, it behaves like `generate_label`.

- [ ] **Step 1:** Tests: profile 1 with `default_printer` = `ql810` and an admin agent reporting `ql810` → dialog has a form to `/label/print` with hidden `printer` = `ql810`; part page menu has the POST icon; without the agent → PR 1 markup only (Review Focus 4). Table action `print_label` with profile 1 and parts 1,2 → one job with `element_ids` `1,2`. Run → FAIL.
- [ ] **Step 2:** Implement. Run → PASS. lint:twig. Commit `feat(labels): print buttons send jobs to agent printers`.

### Task 5: Print agents page and default printer field

**Files:** Create `src/Controller/AgentController.php`, `templates/agents/index.html.twig`; modify `src/Services/Trees/ToolsTreeBuilder.php` (PR 1's "Print agent" node points to the new page, which also hosts PR 1's browser connection section), `src/Form/AdminPages/LabelProfileAdminForm.php` (or the profile form in use) for `default_printer`. Test: `tests/Controller/AgentControllerTest.php`.

- `GET /agents` (`agents_index`): the user's agents (name, version, online/offline + last seen, printers with any reported `media`/`status` shown as plain text), last 50 jobs (time, printer, profile id, ids, status – `unknown` when stale, message) with **Send again** (POST `agents_job_resend/{id}` → new job, same payload) and **Cancel** for pending jobs; **Remove** agent (POST, CSRF). PR 1's browser-connection block is included on this page.
- Profile form: `default_printer` as a `ChoiceType` of printer ids from `printersFor(current user)` plus the current value, `required: false`, placeholder "none"; help text explains it's the printer id reported by the agent.

- [ ] **Step 1:** Tests: page lists an admin agent and its job; resend creates a second job; cancel moves pending → cancelled; remove deletes agent and jobs; another user's agent not listed and its actions → 404; profile edit page shows the select. Run → FAIL. **Step 2:** Implement. **Step 3:** PASS, lint:twig, availability test entry `/agents`. Commit `feat(agents): print agents page and default printer per profile`.

### Task 6: Docs and translations

- [ ] `docs/usage/labels.md`: "Print jobs" section – how an agent registers (any API token, read-only is enough), the four endpoints with request/response examples from Task 2, job lifecycle, never re-queued, 30-day pruning. `translation:touch-catalog messages en` via `dc`. phpstan. Commit `docs(labels): print agent job API`.

### Task 7: pdb-print – agent loop

**Files (agent repo):** modify `internal/partdb/partdb.go` (+ test), `internal/config/config.go` (`AgentID string yaml:"agent_id"`, generated with crypto/rand UUIDv4 and saved on first `serve`), create `internal/source/agentjobs/agentjobs.go` (+ test), modify `cmd/pdb-print/main.go`, `internal/server/server.go` (extract the shared print path), README.

**Interfaces:**
- `partdb.Client`: `Heartbeat(ctx, uuid string, hb Heartbeat) (pollSeconds int, err error)`; `PendingJobs(ctx, uuid) ([]Job, error)`; `ClaimJob(ctx, id int) (bool, error)` (false on 409); `ReportJob(ctx, id int, ok bool, message string) error`. `ErrNoAgentAPI` when the heartbeat returns 404. Types `Heartbeat{Name, Version string; Printers []AgentPrinter}`, `AgentPrinter{ID, Name, Model, Media string}`, `Job{ID int; Type, Printer string; Payload struct{ProfileID int "profile_id"; ElementType string "element_type"; ElementIDs string "element_ids"}}`.
- `agentjobs.Loop{Client, AgentID, Name, Version, Printers func() []partdb.AgentPrinter, Print func(ctx, printerID string, profile int, ids, typ string) (string, error), Log}` with `Run(ctx) error`: heartbeat → list → for each job: claim; if claimed, print, report `done` with the result text or `failed` with the error; sleep `poll` seconds. `ErrNoAgentAPI` → log once at info, retry hourly. Unknown job type → `failed` "unsupported job type".
- `server.(*Server).PrintLabels(ctx, instanceID, printerID string, profile int, ids, typ, source string) (string, error)` used by both handoff and the loop; records the job in the UI history (source `"Part-DB job"`).

- [ ] **Step 1:** Tests: partdb client against an httptest Part-DB (paths, auth header, JSON, 404 → ErrNoAgentAPI, 409 → false); loop against a fake Part-DB with one job → claim, print func called with the payload, result `done` posted; print error → `failed` with message; a job claimed elsewhere (409) → not printed; no agent API → no prints and no crash. Run → FAIL.
- [ ] **Step 2:** Implement; wire in `serve` for every instance with a token. Run `go test ./...`, vet, staticcheck → PASS. Commit `feat: fetch print jobs from Part-DB`.

### Task 8: End-to-end and checks

- [ ] Dev Part-DB with fixtures, `pdb-print serve` (fake printer, `allow_scaling`), agent registers (visible on `/agents`), set profile 1 default printer `fake`, then via headless Chrome: menu icon → job → printed; dialog split button → printed; parts table action with 3 parts → 3 labels; stop the agent → print → "offline" flash, start agent → job printed; Send again works.
- [ ] Part-DB: phpunit (all new tests + LabelControllerTest + availability), phpstan, lint:twig, lint:yaml, `doctrine:schema:validate`. Agent: go test, vet, staticcheck.
- [ ] Draft PR bodies `pr2-body.md` (Part-DB) and `pdbprint-pr-body.md` in the scratchpad; don't post.
