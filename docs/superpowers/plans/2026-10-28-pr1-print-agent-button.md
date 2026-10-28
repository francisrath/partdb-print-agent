# PR 1 – Print button to a local agent – Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One-click label printing from Part-DB to a local print agent, plus one-click setup from the agent's UI.

**Architecture:** Part-DB stores the agent URL and pairing key per browser in `localStorage` (set on a new `/label/agent` page from a link the agent hands out). A Stimulus controller shows Print buttons only when connected and opens the agent's existing `/v1/handoff` with an HMAC-signed query, the same contract as today's bookmarklet. `pdb-print` gets the "Connect to Part-DB" link and accepts longer id lists for bulk prints.

**Tech Stack:** Part-DB 2.20.4 (Symfony, Twig, Stimulus, Bootstrap 5, PHPUnit) in its Docker dev env; `pdb-print` (Go, `html/template`).

**Spec:** `docs/superpowers/specs/2026-10-28-partdb-agent-prs-design.md` (section "PR 1").

## Global Constraints

- Part-DB work in `~/git/Part-DB-server`, branch `feat/print-agent-button` from `master` (462d36c1). Agent work in this repo, branch `feat/partdb-print-button`. **No push, no fork, no PR** until the user says so.
- Run Part-DB commands as `docker compose -f compose.dev.yaml run --rm partdb <cmd>` (abbreviated `dc <cmd>` below).
- Every user-facing string in Part-DB is a translation key in `translations/messages.en.xlf`; afterwards run `dc php bin/console translation:touch-catalog messages en`. No strings hardcoded in Twig or JS (JS only toggles pre-rendered elements).
- Part-DB checks before finishing: `dc vendor/bin/ecs check src/`, `dc composer phpstan`, `dc php bin/console lint:twig templates --env=prod`, the touched PHPUnit tests.
- No database changes in PR 1.
- Handoff contract (unchanged): `GET <agent>/v1/handoff?src&type&ids&profile&ts&sig`; `src` = Part-DB `location.origin`; `type` ∈ `part|part_lot|storelocation`; `ts` = Unix seconds; `sig` = lowercase hex HMAC-SHA256(key, `src\ntype\nids\nprofile\nts`). Unsigned requests make the agent ask for a click.
- Never copy Part-DB (AGPL) code into `pdb-print`; the JS we write for Part-DB is new code.
- Commits: conventional, ending `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A connect link with a non-http(s) URL (`javascript:`, `data:`) or no key must not be stored – the page shows an error and no Connect button. → Task 2 manual check + JS guard.
2. A browser that is not connected must show no Print button and no printer icons at all (no dead buttons). → Task 3/4 manual check; markup rendered with `d-none`.
3. Popup blockers: the window must be opened synchronously in the click handler, before the async WebCrypto signing, else the handoff is blocked. → Task 3 manual check in Chrome.
4. Bulk print of ~300 part ids (`1,2,3,…`) must reach the agent: raise the agent's ids limit. → Task 5 test.
5. Plain-http Part-DB (no `crypto.subtle`) must still work: send unsigned, agent asks for a click. → Task 3 code path, Task 6 E2E on `http://localhost:8080` (localhost is a secure context, so test unsigned by forcing it once via devtools or skip with a note).

---

### Task 1: Branches and connect-page route

**Files:**
- Modify: `src/Controller/LabelController.php` (new action)
- Create: `templates/label_system/agent.html.twig`
- Modify: `translations/messages.en.xlf`
- Test: `tests/ApplicationAvailabilityFunctionalTest.php` (add `yield ['/label/agent'];` next to the `/label/dialog` entries)

**Interfaces:**
- Produces: route `label_agent` at `GET /label/agent`, guarded by `@labels.create_labels`; template block with a root element `data-controller="pages--print-agent-connect"`.

- [ ] **Step 1:** `git -C ~/git/Part-DB-server switch -c feat/print-agent-button`; `git -C /Users/fran/git/partdb-print-agent switch -c feat/partdb-print-button`.
- [ ] **Step 2:** Add the `yield ['/label/agent'];` line; run `dc php bin/phpunit tests/ApplicationAvailabilityFunctionalTest.php --filter label` → FAIL (404). If the test DB isn't set up, follow `.github/workflows/tests.yml` (create test DB + load fixtures) first and note the commands in the plan.
- [ ] **Step 3:** `agent(): Response` action, `#[Route(path: '/agent', name: 'label_agent')]`, renders `label_system/agent.html.twig` (extends `main_card.html.twig` like `dialog.html.twig`). Template sections, all text via new keys under `label_generator.agent.*`: title; explanation (what an agent is, link to Part-DB docs anchor added in Task 4); "connected to <url>" state with Disconnect button; "connect to <url>?" state with Connect button; "invalid link" error; "not connected" hint. Each state is a target hidden with `d-none` until the controller picks it.
- [ ] **Step 4:** Run the test → PASS. `dc php bin/console lint:twig templates --env=prod` → OK.
- [ ] **Step 5:** Commit `feat(labels): add print agent connect page`.

### Task 2: Connect controller and shared storage helper

**Files:**
- Create: `assets/helpers/print_agent.js`
- Create: `assets/controllers/pages/print_agent_connect_controller.js`

**Interfaces:**
- Produces (`assets/helpers/print_agent.js`):
  - `getAgent(): {url: string, key: string} | null` – reads `localStorage['partdb.print_agent']`, returns null on missing/invalid JSON.
  - `setAgent(url: string, key: string): void`, `clearAgent(): void`.
  - `parseConnectHash(hash: string): {url, key} | null` – parses `#url=…&key=…` (URLSearchParams); null unless `url` parses with `new URL` and protocol is `http:` or `https:`, and `key` is non-empty hex. Strips a trailing `/` from `url`.
  - `handoffUrl(agent, {type, ids, profile}): Promise<string>` – builds the query (`src = location.origin`, `ts`), signs with `crypto.subtle` HMAC-SHA256 when available, omits `sig` otherwise.
- Connect controller: targets `connected`, `confirm`, `invalid`, `none`, `url` (text spans); actions `connect`, `disconnect`. On `connect()` (Stimulus lifecycle): if `location.hash` parses → show `confirm` with the URL; if a hash is present but invalid → `invalid`; else show `connected` or `none` from `getAgent()`. `connect` button → `setAgent`, clear the hash with `history.replaceState`, show `connected`. `disconnect` → `clearAgent`, show `none`.

- [ ] **Step 1:** Write both files.
- [ ] **Step 2:** `dc yarn build` (assets service) → compiles without errors.
- [ ] **Step 3:** Manual check at `http://localhost:8080/label/agent#url=http://127.0.0.1:8631&key=ab12` → confirm state; Connect → connected, hash gone; reload → still connected; `#url=javascript:alert(1)&key=ab` → invalid; Disconnect → none.
- [ ] **Step 4:** Commit `feat(labels): store the print agent connection per browser`.

### Task 3: Print button in the label dialog

**Files:**
- Create: `assets/controllers/pages/print_agent_print_controller.js`
- Modify: `templates/label_system/dialog.html.twig` (the `{% if pdf_data %}` button row)
- Modify: `translations/messages.en.xlf`
- Test: `tests/Controller/LabelControllerTest.php` (new, WebTestCase, log in as `admin` like the existing controller tests)

**Interfaces:**
- Consumes: `getAgent`, `handoffUrl` from Task 2.
- Produces: controller `pages--print-agent-print` with values `type: String`, `ids: String`, `profile: Number`; action `print`. On `connect()` it removes `d-none` from its element when `getAgent()` is non-null. `print(event)`: `preventDefault`, `const w = window.open('', 'pdb-print', 'width=420,height=320')` synchronously, then `w.location.href = await handoffUrl(...)`; if `w` is null fall back to `location.href`.

- [ ] **Step 1:** Write `LabelControllerTest::testDialogWithProfileHasPrintButton` – GET `/label/1/dialog?target_id=1&target_type=part&generate=1` → 200 and the response contains `data-controller="pages--print-agent-print"`, `data-pages--print-agent-print-profile-value="1"`, `data-pages--print-agent-print-ids-value="1"`, `data-pages--print-agent-print-type-value="part"`. And `testDialogWithoutProfileHasNoPrintButton` – GET `/label/dialog?target_id=1&target_type=part` → no `pages--print-agent-print`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Button (`btn btn-primary d-none`, `fa-print` icon, key `label_generator.print`, title key `label_generator.print.title` = "Print with your local print agent (uses the saved profile)") next to Download, only when `profile` is set; values from `profile.id`, `form.target_id.vars.value`, `profile.options.supportedElement.value`. Write the controller.
- [ ] **Step 4:** Run tests → PASS; `lint:twig` OK; `yarn build` OK.
- [ ] **Step 5:** Commit `feat(labels): print button for the local print agent`.

### Task 4: Printer icon in the "Label ▾" menu, Tools entry, docs

**Files:**
- Modify: `templates/label_system/dropdown_macro.html.twig`
- Modify: `src/Services/Trees/ToolsTreeBuilder.php` (node after the label dialog, `@labels.create_labels`, icon `fa-print`, key `tree.tools.tools.print_agent`)
- Modify: `docs/usage/labels.md` (new section "Printing with a print agent": setup link format, handoff contract from Global Constraints, security note: key lives in this browser's localStorage)
- Modify: `translations/messages.en.xlf`
- Test: `tests/Controller/LabelControllerTest.php`

- [ ] **Step 1:** Test `testPartPageDropdownHasPrintIcons` – GET `/part/1` → contains `pages--print-agent-print` with `profile-value` of a dropdown profile (fixture "Profile 1", id 1) and `ids-value="1"`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Each profile row becomes `d-flex` with the existing link plus a `d-none` icon button (`btn btn-link btn-sm`, `fa-print`, title key `label_generator.print`) using the print controller; only when `id` is not null. Tools tree node; docs section.
- [ ] **Step 4:** Tests → PASS; also run `tests/ApplicationAvailabilityFunctionalTest.php --filter "tree|part"` → PASS.
- [ ] **Step 5:** `dc php bin/console translation:touch-catalog messages en`; commit `feat(labels): print icons in the label menu`.

### Task 5: pdb-print – connect link and longer id lists

**Files (this repo):**
- Modify: `internal/server/server.go` (index view data; handoff ids limit)
- Modify: `internal/server/web/index.html` (link next to the bookmarklets)
- Modify: `README.md`, `CLAUDE.md` (decision line about upstream work)
- Test: `internal/server/server_test.go`

**Interfaces:**
- Produces: index view field `ConnectURL string` = `<instance BaseURL>/label/agent#url=<url.QueryEscape("http://"+r.Host)>&key=<pairing key>`, empty when there is no instance or key.

- [ ] **Step 1:** Tests: `TestIndexConnectLink` – index page contains `/label/agent#url=http%3A%2F%2F` + the test host and `&amp;key=` + key; `TestHandoffLongIDs` – a signed GET with `ids` = `1,2,…,300` joined (≈1.1 kB) is accepted (prints to the virtual/fake printer), and > 8 kB is rejected with "Invalid element ids.".
- [ ] **Step 2:** `go test ./internal/server/` → FAIL.
- [ ] **Step 3:** Raise the limit to 8192; add `ConnectURL`; link text "Connect Part-DB to this agent" with a one-line hint (needs Part-DB with the Print button).
- [ ] **Step 4:** `go test ./...`, `go vet ./...`, `staticcheck ./...` → PASS.
- [ ] **Step 5:** Commit `feat(server): connect link for Part-DB's print button`.

### Task 6: End-to-end check and Part-DB checks

- [ ] **Step 1:** Start Part-DB dev (`docker compose -f compose.dev.yaml up -d partdb assets`), log in as admin, create label profile "29 mm bag" for parts (29×46 mm) and a read-only API token.
- [ ] **Step 2:** Run `pdb-print serve` with a scratch config: instance `http://localhost:8080` + that token, printer `virtual` (PNG output in the scratchpad), route profile → virtual printer.
- [ ] **Step 3:** In Chrome: agent UI → "Connect Part-DB" → Connect → part page → Label ▾ printer icon → handoff window prints without a click and closes; PNG appears. Dialog Print button the same. Parts table: select 3 parts → Generate labels → Print → 3 labels. One real label on the QL-810W with the user's own Part-DB is optional (user allowed "print a bit").
- [ ] **Step 4:** `dc vendor/bin/ecs check src/`, `dc composer phpstan`, `dc php bin/console lint:twig templates --env=prod`, `dc php bin/phpunit tests/Controller/LabelControllerTest.php tests/ApplicationAvailabilityFunctionalTest.php` → all clean.
- [ ] **Step 5:** Write the draft PR body to the scratchpad (`pr1-body.md`): what, why (#1625), screenshots, how to test with or without pdb-print, "alternative to PR 2/3". Show the user; don't post.
