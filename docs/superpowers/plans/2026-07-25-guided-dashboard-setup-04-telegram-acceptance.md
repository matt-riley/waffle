# Guided Dashboard Setup: Telegram and Integrated Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator verify, enable, and pair a Telegram bot from Desk or `waffle setup telegram`, then prove the complete Models, Skills, GitHub, and Telegram onboarding experience on the managed Safari target.

**Architecture:** Extract a credential-safe Telegram `getMe` verifier from the existing adapter, add Telegram configuration to `internal/setupconfig` over the shared config transaction engine, and project only private-chat pairing records through the Setup service. Desk and CLI call the same domain operations; the final tasks update all onboarding documentation and run the full local and managed acceptance matrix.

**Tech Stack:** Go 1.25.12, stdlib Telegram Bot API client, shared `internal/configtxn`, `internal/setupconfig`, SQLite entity store, templ, embedded JavaScript, Safari via Computer Use.

## Global Constraints

- Telegram remains optional and never changes Waffle's Installed/Ready lifecycle.
- Validate every token with `getMe` before committing it.
- Store the token only as `telegram/bot-token`; config contains only `secret://telegram/bot-token`.
- Enabling Telegram is one config/secret transaction followed by normal restart and health confirmation.
- Restart or health failure restores previous config, secret, and service state.
- Only pending Telegram private-chat pairings are shown or approvable in Desk.
- Group, supergroup, and channel strangers remain silently ignored and never create pairing requests.
- Pair approval preserves the existing single-owner identity semantics and audit posture.
- Bot tokens, raw Telegram bodies, environment names, secret references, and internal errors never cross HTTP or enter logs.
- Existing `waffle secret set telegram/bot-token`, explicit config, and `waffle pair` remain recovery/automation paths.
- Desk remains loopback-only with request-token, idempotency, Host, Origin, fetch-metadata, CSP, no-store, body-limit, focus, keyboard, and reduced-motion enforcement.

---

### Task 1: Extract a safe Telegram bot verifier

**Files:**
- Create: `internal/channel/telegram/verify.go`
- Create: `internal/channel/telegram/verify_test.go`
- Modify: `internal/channel/telegram/telegram.go`
- Modify: `internal/channel/telegram/telegram_test.go`

**Interfaces:**
- Consumes: the adapter's current Bot API request/response encoding and
  `DefaultBaseURL`.
- Produces:

```go
type BotIdentity struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Verifier interface {
	Verify(context.Context, token, baseURL string) (BotIdentity, error)
}

type HTTPVerifier struct {
	Client *http.Client
}

var ErrTokenInvalid = errors.New("telegram token invalid")
```

- [ ] **Step 1: Write verifier tests**

Use `httptest.Server` to cover valid `getMe`, `ok:false`, 401, 404, 429,
malformed JSON, missing bot ID/username, response over 64 KiB, timeout,
cancellation, invalid base URL, redirect, and token redaction.

The happy response is:

```json
{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Waffle","username":"waffle_bot"}}
```

Assert the token is present only in the request path expected by the Bot API
and never in errors or returned identity.

- [ ] **Step 2: Run verifier tests and verify they fail**

```sh
go test ./internal/channel/telegram -run 'TestVerifier' -count=1
```

Expected: FAIL because the exported verifier does not exist.

- [ ] **Step 3: Implement the verifier and share decoding with the adapter**

Factor bounded `getMe` decoding into an unexported function used by both
`HTTPVerifier.Verify` and `Adapter.ensureBot`. The default verifier client uses
a ten-second timeout and rejects redirects. Map any authentication failure to
`ErrTokenInvalid`; transient/network failures retain a sanitized wrapper.

- [ ] **Step 4: Preserve adapter behavior**

Keep long-polling client behavior, cached username, mention gating, and health
observer unchanged. Existing adapter tests must pass byte-for-byte except where
they deliberately use the new shared decoder.

- [ ] **Step 5: Run Telegram tests**

```sh
go test ./internal/channel/telegram -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit Telegram verification**

```sh
git add internal/channel/telegram
git commit -m "refactor: share telegram bot verification"
```

---

### Task 2: Add transactional Telegram configuration

**Files:**
- Create: `internal/setupconfig/telegram.go`
- Create: `internal/setupconfig/telegram_test.go`
- Modify: `internal/setupconfig/manager.go`
- Modify: `internal/setupconfig/toml.go`
- Modify: `internal/setupconfig/toml_test.go`
- Modify: `internal/configtxn/engine_test.go`

**Interfaces:**
- Consumes: `configtxn.Engine`, `telegram.Verifier`, and canonical config TOML.
- Produces:

```go
type TelegramStatus struct {
	Configured bool                  `json:"configured"`
	Enabled    bool                  `json:"enabled"`
	Verified   bool                  `json:"verified"`
	Healthy    bool                  `json:"healthy"`
	Bot        *telegram.BotIdentity `json:"bot,omitempty"`
	Summary    string                `json:"summary"`
}

type TelegramRequest struct {
	Token   string
	BaseURL string
}

func (m *Manager) TelegramStatus(context.Context, bool) (TelegramStatus, error)
func (m *Manager) ConfigureTelegram(context.Context, TelegramRequest, configtxn.Mode) (TelegramStatus, configtxn.Result, error)
func (m *Manager) VerifyTelegram(context.Context) (TelegramStatus, error)
```

Task 2 adds `Telegram telegram.Verifier` to the existing
`setupconfig.Manager`; the GitHub verifier and methods from Plan 03 remain
unchanged.

`TelegramStatus` resolves the configured secret inside a bounded transaction
read and performs `getMe` when Telegram is configured. A verification failure
returns a typed row-status error; the dashboard converts it to optional
`needs_attention` without failing the complete Setup snapshot. This makes the
verified bot identity available after a process restart or browser reload
without persisting Telegram profile data in config.

- [ ] **Step 1: Write configuration and rollback tests**

Cover disabled/missing token, enabled/missing secret, configured unverified,
healthy status input, valid token, invalid token before staging, custom base
URL, canonical TOML, preserving unrelated config, secret-before-config commit,
deferred restart, every injected failure boundary, previous enabled token
restoration, first-install file absence restoration, and redaction.

The committed config is exactly:

```toml
[channel.telegram]
enabled = true
token = "secret://telegram/bot-token"
```

`base_url` is added only when explicitly supplied.

- [ ] **Step 2: Run setupconfig Telegram tests and verify they fail**

```sh
go test ./internal/setupconfig -run 'TestTelegram' -count=1
```

Expected: FAIL because Telegram setup methods do not exist.

- [ ] **Step 3: Add canonical Telegram TOML mutation**

Implement:

```go
func SetTelegram(raw []byte, cfg config.Telegram) ([]byte, error)
```

Reject duplicate/noncanonical managed tables. Preserve unrelated TOML and
comments. Never accept a raw token in `config.Telegram.Token`.

- [ ] **Step 4: Implement validate-then-commit configuration**

Call `Verifier.Verify` with the request token before `Txn.Apply`. The plan
contains one `SecretOp` for `telegram/bot-token` and candidate config with the
secret reference. Copy/clear token bytes on every path. Return the verified bot
identity only in the immediate response; later verification derives it again.

- [ ] **Step 5: Run setup, transaction, config, and Telegram tests**

```sh
go test ./internal/configtxn ./internal/setupconfig ./internal/config ./internal/channel/telegram -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit transactional Telegram setup**

```sh
git add internal/setupconfig internal/configtxn
git commit -m "feat: configure telegram transactionally"
```

---

### Task 3: Add private Telegram pairing operations to Setup

**Files:**
- Create: `internal/dashboard/setup_telegram.go`
- Create: `internal/dashboard/setup_telegram_test.go`
- Modify: `internal/dashboard/setup.go`
- Modify: `internal/dashboard/setup_test.go`
- Modify: `internal/dashboard/api.go`
- Modify: `internal/dashboard/router.go`
- Modify: `internal/entity/entity.go`
- Modify: `internal/entity/entity_test.go`
- Modify: `cmd/waffle/dashboard_wiring.go`
- Modify: `cmd/waffle/dashboard_wiring_test.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/serve_cmd_test.go`

**Interfaces:**
- Consumes: `setupconfig.Manager`, observability adapter health,
  `entity.Store.Pairings`, `entity.Store.Approve`, restart scheduler.
- Produces:

```go
type TelegramPairing struct {
	Code       string    `json:"code"`
	ExternalID string    `json:"external_id"`
	SenderName string    `json:"sender_name"`
	CreatedAt  time.Time `json:"created_at"`
}

type TelegramPairings interface {
	TelegramPairings(context.Context) ([]TelegramPairing, error)
	ApproveTelegram(context.Context, code, name string) (*entity.Identity, error)
}
```

Routes:

```text
POST /api/v1/desk/setup/telegram
POST /api/v1/desk/setup/telegram/verify
POST /api/v1/desk/setup/telegram/pairings/{code}/approve
```

- [ ] **Step 1: Write entity filtering and approval tests**

Add a narrow entity method:

```go
func (s *Store) PairingsForChannel(context.Context, string) ([]Pairing, error)
```

Test it returns only Telegram records in creation order. Retain the gateway
invariant test proving group messages never call `Pair`; therefore every
Telegram pending record originates from a private chat. Approval must bind the
same channel/external ID, delete only the selected code, be transactional, and
return not-found after replay.

- [ ] **Step 2: Write Setup HTTP tests**

Cover all Telegram row states, sanitized bot identity, pairing list,
configure/verify/approve, strict JSON, uppercase six-character code validation,
request token, idempotency, restart scheduling, adapter health projection,
token clearing, no `chat_id` exposure, and stable errors:

```go
var telegramHTTPErrorCases = []struct {
	err    error
	status int
	code   string
}{
	{telegram.ErrTokenInvalid, http.StatusUnauthorized, "telegram_token_invalid"},
	{configtxn.ErrLocked, http.StatusConflict, "setup_locked"},
	{configtxn.ErrDeferredHealth, http.StatusServiceUnavailable, "telegram_restart_failed"},
	{entity.ErrPairingNotFound, http.StatusNotFound, "pairing_not_found"},
}
```

Introduce `entity.ErrPairingNotFound` and wrap it in `Approve`; stop relying on
formatted string matching.

- [ ] **Step 3: Run tests and verify they fail**

```sh
go test ./internal/entity ./internal/dashboard ./cmd/waffle -run 'Test(TelegramPair|SetupTelegram)' -count=1
```

Expected: FAIL because filtered pairing/status routes do not exist.

- [ ] **Step 4: Implement Telegram Setup routes and snapshot projection**

The configure handler clears token values in `defer`, calls
`ConfigureTelegram(..., configtxn.AwaitRestart)`, and returns `202`. Verify and
pair approval return `200` without restart. Snapshot action is `connect`,
`repair`, `pair`, or `verify`; the row is always `required: false`.

The bot link is derived client-side only from the sanitized username:
`https://t.me/<url-escaped-username>`.

- [ ] **Step 5: Wire entity, health, and setupconfig dependencies**

Pass the existing `entities` store and `observability.Service` into the Setup
service. Do not create a second DB/store or health poller. The shared
transaction engine remains the single deferred finalization owner.

- [ ] **Step 6: Run dashboard, entity, serve, and gateway tests**

```sh
go test ./internal/entity ./internal/gateway ./internal/dashboard ./internal/setupconfig ./cmd/waffle -count=1
```

Expected: PASS, including group-origin pairing denial.

- [ ] **Step 7: Commit Telegram Setup routes**

```sh
git add internal/entity internal/dashboard cmd/waffle
git commit -m "feat: expose telegram setup and pairing"
```

---

### Task 4: Build the Telegram Desk task

**Files:**
- Modify: `internal/dashboard/ui/setup.templ`
- Modify: `internal/dashboard/ui/setup_templ.go`
- Modify: `internal/dashboard/ui/assets/setup.js`
- Modify: `internal/dashboard/ui/assets/setup.css`
- Modify: `internal/dashboard/ui/setup_ui_test.go`
- Modify: `internal/dashboard/ui/setup_client_test.mjs`

**Interfaces:**
- Consumes: Telegram status, bot identity, pairings, and routes from Task 3.
- Produces: token setup, bot confirmation, private-message instruction, pending
  pairing, and approval states.

- [ ] **Step 1: Write client tests for the complete flow**

Assert:

- BotFather instruction links to `https://t.me/BotFather`;
- token input is password/autocomplete-off and clears after success, failure,
  Back, and reload;
- verified bot first name and `@username` render as text;
- the `t.me` link is constructed only from the sanitized username;
- restart polling transitions to health/pairing state;
- an empty list asks the owner to send a private message;
- pending pairings show sender, external ID, six-character code, and time but
  not raw chat ID;
- **Approve as me** requires an explicit click and refreshes canonical state;
- group pairings cannot render because the API never returns them; and
- token or upstream error text never appears in DOM/storage.

- [ ] **Step 2: Run UI tests and verify they fail**

```sh
go test ./internal/dashboard/ui -run 'TestSetupTelegram' -count=1
node --test --test-name-pattern='telegram' internal/dashboard/ui/setup_client_test.mjs
```

Expected: FAIL because the Telegram task does not exist.

- [ ] **Step 3: Implement token, identity, and restart states**

The task first explains BotFather, then collects the token. On immediate
validation success show the bot identity; after restart polling, reload Setup
and show adapter health. If restart scheduling returns manual restart guidance,
display that safe instruction without losing the configured status.

- [ ] **Step 4: Implement pairing list and approval**

Render each pairing as an article with a real button. Approval posts no
client-selected channel or external ID—only the path-bound code and optional
owner display name. Move focus to the success heading after approval and
announce the recognized identity.

- [ ] **Step 5: Run UI, HTTP, and domain tests**

```sh
go test ./internal/dashboard/ui ./internal/dashboard ./internal/setupconfig ./internal/entity -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit the Telegram Desk task**

```sh
git add internal/dashboard/ui
git commit -m "feat: guide telegram setup and pairing"
```

---

### Task 5: Extend `waffle setup` with Telegram onboarding

**Files:**
- Create: `cmd/waffle/setup_telegram.go`
- Create: `cmd/waffle/setup_telegram_test.go`
- Modify: `cmd/waffle/setup_cmd.go`
- Modify: `cmd/waffle/setup_cmd_test.go`
- Modify: `cmd/waffle/completion_cmd.go`

**Interfaces:**
- Consumes: `setupconfig.Manager`, existing hidden/piped secret reader, and
  `entity.Store`.
- Produces:

```text
waffle setup telegram
```

- [ ] **Step 1: Write CLI tests**

Cover help, invalid options, valid/invalid token, configured rerun, custom
advanced Base URL, restart/reconcile failure, output redaction, no-echo reader,
bot identity output, zero/multiple pending pairings, approve prompt, explicit
skip, and compatibility with `waffle pair`.

- [ ] **Step 2: Run CLI tests and verify they fail**

```sh
go test ./cmd/waffle -run 'TestSetupTelegram' -count=1
```

Expected: FAIL because the subcommand does not exist.

- [ ] **Step 3: Route the subcommand and reuse setupconfig**

Dispatch `setup telegram` from `setupCmd`, construct the same domain manager as
Desk, and use `configtxn.Reconcile` because the standalone CLI owns lifecycle
callbacks directly. Bare `waffle setup` remains compatible and prints Telegram
as an optional next action.

- [ ] **Step 4: Implement prompts and pairing handoff**

After verification print:

```text
Telegram bot verified: Waffle (@waffle_bot)
Send it a private message, then run `waffle pair ls` or approve in Waffle Desk.
```

If pairings already exist, list only Telegram pairings and offer one-at-a-time
approval. Never auto-approve.

- [ ] **Step 5: Run CLI, setup, Telegram, and entity tests**

```sh
go test ./cmd/waffle ./internal/setupconfig ./internal/channel/telegram ./internal/entity -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit CLI Telegram setup**

```sh
git add cmd/waffle
git commit -m "feat: guide telegram setup in cli"
```

---

### Task 6: Complete onboarding documentation

**Files:**
- Modify: `docs/waffle-desk.md`
- Modify: `docs/chat.md`
- Modify: `docs/deploy.md`
- Modify: `config.example.toml`
- Modify: documentation assertions in existing Go tests where applicable.

**Interfaces:**
- Consumes: completed Models, Skills, GitHub, Telegram, Desk, and CLI behavior.
- Produces: one coherent operator journey with advanced recovery separated.

- [ ] **Step 1: Rewrite Desk onboarding documentation**

Lead with:

```text
Open Waffle Desk → Setup
```

Describe Required/Optional semantics and the four status rows. Document the
focused model/provider, Skills, GitHub, and Telegram paths. State that
`section=capabilities` remains the stable URL even though the visible
destination is Setup.

- [ ] **Step 2: Update CLI and deployment recovery documentation**

Document:

```sh
waffle setup
waffle setup github
waffle setup telegram
waffle secret set github/token
waffle secret set telegram/bot-token
waffle pair ls
waffle pair approve <code>
```

Explain App config, skill source policy, exact commit provenance, zip bounds,
inactive installation, activation, restart behavior, and rollback. Keep raw
TOML below the guided path.

- [ ] **Step 3: Update example configuration**

Retain advanced controls, but comment that ordinary onboarding happens in Desk
or `waffle setup`. Include labeled skill import roots, GitHub App recovery
shape, and Telegram recovery shape without any real secrets.

- [ ] **Step 4: Run documentation tests and diff checks**

```sh
go test ./internal/providerconfig ./internal/config ./cmd/waffle -run 'Test.*Docs|Test.*Config' -count=1
git diff --check
```

Expected: PASS.

- [ ] **Step 5: Commit complete onboarding documentation**

```sh
git add docs/waffle-desk.md docs/chat.md docs/deploy.md config.example.toml internal/providerconfig internal/config cmd/waffle
git commit -m "docs: guide waffle setup onboarding"
```

---

### Task 7: Run the full automated release gate

**Files:**
- Modify only for scoped failures proven by this gate.

**Interfaces:**
- Consumes: all four implementation plans.
- Produces: repository-wide automated evidence.

- [ ] **Step 1: Format generated and handwritten files**

```sh
mise run fmt
git diff --check
```

Expected: PASS with no generated templ drift.

- [ ] **Step 2: Run focused security and behavior suites**

```sh
go test ./internal/configtxn ./internal/providerconfig ./internal/skillinstall ./internal/gitcred ./internal/channel/telegram ./internal/setupconfig ./internal/entity ./internal/gateway ./internal/dashboard ./internal/dashboard/ui ./cmd/waffle -count=1
node --test internal/dashboard/ui/*_client_test.mjs
```

Expected: PASS.

- [ ] **Step 3: Run standard project gates**

```sh
mise run dashboard-check
mise run vet
mise run lint
mise run test
mise run build
```

Expected: every command passes.

- [ ] **Step 4: Verify secret absence**

Seed unique canary values for provider key, GitHub PAT, App PEM body, and
Telegram token in isolated tests, exercise every success and failure endpoint,
then search captured HTTP bodies, logs, policy audit rows, config, generated
assets, and error trees. The test fails if any canary appears outside the
encrypted secret-store fixture.

- [ ] **Step 5: Commit any gate-driven fixes**

If needed:

```sh
git add internal/configtxn internal/providerconfig internal/skillinstall internal/gitcred internal/channel/telegram internal/setupconfig internal/entity internal/gateway internal/dashboard cmd/waffle
git commit -m "fix: close setup verification gaps"
```

Do not create an empty commit.

---

### Task 8: Prove the complete goal on managed Safari

**Files:**
- Create: `docs/verification/2026-07-25-guided-dashboard-setup.md`
- Add screenshots only under the existing ignored/local evidence location;
  do not commit credential-bearing captures.

**Interfaces:**
- Consumes: deployed candidate binary, managed Waffle service, forwarded
  loopback Desk, real or dedicated test integrations.
- Produces: requirement-by-requirement completion evidence.

- [ ] **Step 1: Deploy the candidate through the existing managed process**

Build the version-stamped binary, install through the project's normal managed
deployment path, restart `waffle.service`, and record binary version, config
generation, service health, and sanitized logs. Do not paste credentials into
commands, environment, shell history, or the verification document.

- [ ] **Step 2: Prove Models in Safari**

Using Computer Use with Safari:

1. open `/desk/?section=capabilities` and verify the visible destination is
   Setup;
2. refresh the real enrolled provider catalogue;
3. search and keyboard-select a model row;
4. accept/edit its generated alias and optional roles;
5. add it transactionally and wait for process generation change;
6. select it in a real Desk conversation and receive a successful reply.

Capture the resulting sanitized Setup/model state, not the credential entry.

- [ ] **Step 3: Prove Skills in Safari**

Discover a public multi-skill GitHub repository, select a skill folder, verify
the automatically resolved full commit and readable manifest, install inactive,
activate separately, attach from Today, and invoke it in a conversation. Repeat
discovery with a bounded zip. If a labeled host root is configured, prove it
without exposing the path.

- [ ] **Step 4: Prove GitHub in Safari and a workspace**

Configure or verify a repository-scoped credential, prove the credential input
is blank after submission and reload, then exercise authenticated Git inside
the exact bound workspace. Attempt another repository and prove the broker
denies it. Record whether PAT or App was tested and the sanitized repository
scope.

- [ ] **Step 5: Prove Telegram in Safari**

Configure the bot, verify the displayed bot identity, wait for healthy adapter
status, send a private message, see the pending pairing, approve it in Desk,
and receive a bot reply as the recognized owner. Send an addressed message from
an unpaired group identity and prove no pairing is created.

- [ ] **Step 6: Prove optionality, redaction, rollback, and CLI equivalence**

Confirm:

- optional unset/broken integrations do not degrade overall readiness;
- credential fields remain blank after every submission/reload;
- logs and HTTP snapshots contain no credential canaries;
- one injected transaction failure restores previous config, secrets, and
  service health;
- `waffle setup github`, `waffle setup telegram`, provider/secret recovery,
  and `waffle pair` read the same resulting state.

- [ ] **Step 7: Write and commit the verification record**

The record maps every acceptance item to date, environment, exact command or
Safari action, observed result, and sanitized evidence location. It explicitly
lists any external gate not run; the goal cannot be marked complete while one
remains.

```sh
git add docs/verification/2026-07-25-guided-dashboard-setup.md
git commit -m "test: verify guided dashboard setup"
```

- [ ] **Step 8: Run final completion audit**

Compare the approved design specification section by section against current
code, automated tests, managed service state, Safari behavior, CLI behavior,
and the verification record. Any missing or indirect evidence is incomplete:
fix it and rerun the affected gates before claiming the goal complete.
