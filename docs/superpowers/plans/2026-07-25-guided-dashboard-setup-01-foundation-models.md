# Guided Dashboard Setup: Foundation and Models Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the backend-shaped Capabilities landing page with a persistent Setup status surface and make a refreshed provider catalogue directly selectable for transactional model enrollment.

**Architecture:** Extract the provider manager's crash-safe config/secret lifecycle into `internal/configtxn`, then keep provider-specific TOML and probe behavior in `internal/providerconfig`. Rename the Desk presentation to Setup while retaining `section=capabilities`, expose one sanitized setup snapshot, and implement the model flow as a focused client-side task over the existing typed provider service.

**Tech Stack:** Go 1.25.12, `net/http`, `filippo.io/age`, `go-toml`, templ, embedded CSS/JavaScript, Node's built-in test runner.

## Global Constraints

- Preserve the existing `section=capabilities` route for bookmarks and released clients; its navigation label and page title become **Setup**.
- A probed default model is the only Setup requirement for Waffle's Ready lifecycle.
- GitHub, Telegram, and Skills remain optional and cannot degrade installation readiness.
- Desk remains loopback-only and all mutations retain Host, Origin, fetch-metadata, request-token, idempotency, CSP, and no-store enforcement.
- Credentials appear only in mutation requests, are cleared from the DOM after success or failure, and are never returned or written to browser storage.
- Keep the current warm cream, near-black, ginger, border, shadow, type, motion, and reduced-motion visual system.
- Use `gofmt`; tests stay beside implementation; all errors crossing HTTP are stable and sanitized.
- Do not alter the existing provider CLI or provider transaction semantics while extracting the shared engine.

---

### Task 1: Extract the shared crash-safe config transaction engine

**Files:**
- Create: `internal/configtxn/engine.go`
- Create: `internal/configtxn/engine_test.go`
- Create: `internal/configtxn/files.go`
- Create: `internal/configtxn/journal.go`
- Modify: `internal/providerconfig/manager.go`
- Modify: `internal/providerconfig/manager_test.go`
- Modify: `internal/providerconfig/capabilities_test.go`

**Interfaces:**
- Consumes: `config.Load`, `secret.Store`, `secret.OpenFile`, `instance.Acquire`, and the lifecycle callbacks already held by `providerconfig.Manager`.
- Produces:

```go
package configtxn

type SecretReader interface {
	Get(string) (string, error)
	List() ([]string, error)
}

type Snapshot struct {
	ConfigBytes []byte
	Config      config.Config
	Secrets     SecretReader
}

type Mode uint8

const (
	Reconcile Mode = iota
	AwaitRestart
)

type SecretOp struct {
	Name   string
	Value  string
	Delete bool
}

type Plan struct {
	ConfigBytes []byte
	SecretOps   []SecretOp
	Probe       func(context.Context, config.Config, SecretReader) error
}

type Result struct {
	RestartRequired bool
	TransactionID   string
}

type Engine struct {
	ConfigPath     string
	SecretsPath    string
	LockPath       string
	Identity       *age.X25519Identity
	Random         io.Reader
	Restart        func(context.Context) error
	Stop           func(context.Context) error
	Health         func(context.Context) error
	ServiceActive  func(context.Context) (bool, error)
	RestoreService func(context.Context, bool) error
	AfterCommit    func(resource string) error
	CrashAfterPhase func(phase string) error
}

func (e *Engine) Apply(
	ctx context.Context,
	mode Mode,
	prepare func(context.Context, Snapshot) (Plan, error),
) (Result, error)

func (e *Engine) Read(
	ctx context.Context,
	read func(context.Context, Snapshot) error,
) error

func (e *Engine) FinalizeDeferred(context.Context) error
```

`configtxn.ErrLocked`, `ErrSimulatedCrash`,
`ErrDeferredRestartPending`, `ErrDeferredHealth`, and
`ErrDeferredIntegrity` are the canonical lifecycle sentinels.
`providerconfig` re-exports aliases so its existing callers and tests do not
change error classification.

`Snapshot` exposes immutable copies of `ConfigBytes`, parsed `config.Config`,
and a read-only `SecretReader` over the transaction-private store; it never
exposes staged paths or a mutable live store. `Apply` owns the single host lock, recovery, staging,
secret-before-config commit order, journal, lifecycle reconciliation, rollback,
and final cleanup.

- [ ] **Step 1: Write engine tests for commit order, locking, rollback, crash recovery, and deferred finalization**

Create table-driven tests that use a temporary config and age store. The core
test plan must include:

```go
plan := Plan{
	ConfigBytes: []byte("[channel.telegram]\nenabled = true\ntoken = \"secret://telegram/bot-token\"\n"),
	SecretOps:   []SecretOp{{Name: "telegram/bot-token", Value: "new-token"}},
	Probe: func(_ context.Context, candidate config.Config, staged SecretReader) error {
		if !candidate.Channel.Telegram.Enabled {
			return errors.New("telegram not enabled")
		}
		value, err := staged.Get("telegram/bot-token")
		if err != nil || value != "new-token" {
			return errors.New("staged secret unavailable")
		}
		return nil
	},
}
```

Assert secret commit precedes config commit, each injected failure restores
byte-identical files and service state, a second concurrent `Apply` returns
`configtxn.ErrLocked`, every journal phase recovers, and `AwaitRestart`
finalizes only against the exact committed config generation.

- [ ] **Step 2: Run the new tests and verify they fail**

Run:

```sh
go test ./internal/configtxn -run 'TestEngine' -count=1
```

Expected: FAIL because `internal/configtxn` and its exported engine do not yet
exist.

- [ ] **Step 3: Move the generic file, lock, journal, and lifecycle code into `internal/configtxn`**

Move the behavior currently implemented by `providerconfig.Manager.acquire`,
`capture`, `stageSecrets`, `commit`, `writeBackups`, `recoverLocked`,
`rollbackJournal`, `FinalizeDeferred`, and their durable-file helpers. Keep
provider TOML editing, provider validation, secret naming, model resolution,
and model probes out of the new package.

`Apply` must reject invalid modes before acquiring the lock, parse and validate
`Plan.ConfigBytes`, apply `SecretOps` only to a staged store, invoke `Probe`
against the staged candidate, and redact every `SecretOp.Value` from returned
error trees.

- [ ] **Step 4: Adapt `providerconfig.Manager` without changing its public API**

Add an internal `engine() *configtxn.Engine` adapter and map:

```go
func providerMode(mode CommitMode) configtxn.Mode {
	if mode == CommitForRestart {
		return configtxn.AwaitRestart
	}
	return configtxn.Reconcile
}
```

`AddWithMode`, `AddModelWithMode`, activation, removal, `Snapshot`,
`CatalogSnapshot`, `Preflight`, and `FinalizeDeferred` must continue exposing
their existing signatures and errors. Provider preparation callbacks build the
same canonical TOML bytes and provider secret operations, and their probe
callbacks resolve the staged provider key before calling `Manager.Probe`.

- [ ] **Step 5: Run provider and transaction regression tests**

Run:

```sh
go test ./internal/configtxn ./internal/providerconfig -count=1
```

Expected: PASS, including byte-for-byte rollback, crash-phase recovery,
deferred-generation integrity, redaction, and concurrency tests.

- [ ] **Step 6: Commit the shared transaction extraction**

```sh
git add internal/configtxn internal/providerconfig
git commit -m "refactor: share setup config transactions"
```

---

### Task 2: Add the sanitized Setup snapshot and stable status model

**Files:**
- Create: `internal/dashboard/setup.go`
- Create: `internal/dashboard/setup_test.go`
- Modify: `internal/dashboard/api.go`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/dashboard_wiring.go`
- Modify: `cmd/waffle/dashboard_wiring_test.go`

**Interfaces:**
- Consumes: `CapabilityProviders.Snapshot`, `CapabilitySkills.List`, the
  connection/health sources added by later plans, and server-provided skill
  source capabilities.
- Produces:

```go
type SetupState string

const (
	SetupNotConfigured SetupState = "not_configured"
	SetupConfigured    SetupState = "configured"
	SetupReady         SetupState = "ready"
	SetupNeedsAttention SetupState = "needs_attention"
)

type SetupArea struct {
	State    SetupState `json:"state"`
	Required bool       `json:"required"`
	Summary  string     `json:"summary"`
	Action   string     `json:"action"`
}

type SetupSnapshot struct {
	Readiness string                 `json:"readiness"`
	Areas     map[string]SetupArea   `json:"areas"`
	Providers providerconfig.Listing `json:"providers"`
	Skills    []CapabilitySkill      `json:"skills"`
	Sources   SkillSourceCapabilities `json:"skill_sources"`
}

type PublicImportRoot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type SkillSourceCapabilities struct {
	GitHub     bool               `json:"github"`
	Archive    bool               `json:"archive"`
	LocalRoots []PublicImportRoot `json:"local_roots"`
}

type SetupService struct {
	Capabilities *Capabilities
	Integrations SetupIntegrationSource
	Sources      SkillSourceCapabilitySource
}

type SetupIntegrationSource interface {
	GitHub(context.Context) (SetupArea, error)
	Telegram(context.Context) (SetupArea, error)
}

type SkillSourceCapabilitySource interface {
	SkillSources(context.Context) (SkillSourceCapabilities, error)
}

func (s *SetupService) Snapshot(context.Context, string) (SetupSnapshot, error)
```

An error from either optional integration or the skill-source catalogue maps
only that area to `needs_attention`; it does not fail the snapshot or alter
`Readiness`.

- [ ] **Step 1: Write snapshot tests for readiness and allowlisted JSON**

Test these exact cases:

```go
tests := []struct {
	name      string
	providers providerconfig.Listing
	wantReady string
	wantModel SetupArea
}{
	{
		name:      "no default",
		providers: providerconfig.Listing{State: "installed"},
		wantReady: "installed",
		wantModel: SetupArea{State: SetupNotConfigured, Required: true, Action: "add_model"},
	},
	{
		name: "probed default",
		providers: providerconfig.Listing{
			State: "ready", DefaultModel: "gpt",
			Models: map[string]providerconfig.ModelSummary{"gpt": {Provider: "openai", Model: "gpt-5"}},
		},
		wantReady: "ready",
		wantModel: SetupArea{State: SetupReady, Required: true, Action: "add_model"},
	},
}
```

Marshal the result and assert it excludes `secret://`, `api_key`, raw import
paths, environment names, PEM data, and command strings. Assert optional area
failures never change `Readiness`.

- [ ] **Step 2: Run the snapshot tests and verify they fail**

Run:

```sh
go test ./internal/dashboard -run 'TestSetupSnapshot' -count=1
```

Expected: FAIL because `SetupService` does not exist.

- [ ] **Step 3: Implement `SetupService.Snapshot` and `GET /api/v1/desk/setup`**

Build models and skills from the existing Capabilities service. Define
`SetupIntegrationSource` now with a zero-value implementation returning
optional `not_configured` GitHub and Telegram rows; Plans 03 and 04 replace it
with live status. Return source booleans and sanitized labels only. Register
the GET endpoint without mutation middleware and reuse the existing
one-second provider-lock retry behavior.

- [ ] **Step 4: Wire the Setup service through `APIConfig` and `serve`**

Add `Setup *SetupService` to `dashboard.APIConfig`, mount its route when
non-nil, and construct it beside the existing Capabilities service so the
legacy endpoint remains compatible during the migration.

- [ ] **Step 5: Run focused dashboard tests**

Run:

```sh
go test ./internal/dashboard ./cmd/waffle -run 'Test(Setup|Dashboard)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the Setup status API**

```sh
git add internal/dashboard cmd/waffle/dashboard_wiring.go cmd/waffle/dashboard_wiring_test.go
git commit -m "feat: add sanitized setup status"
```

---

### Task 3: Replace the Capabilities shell with the persistent Setup surface

**Files:**
- Create: `internal/dashboard/ui/setup.templ`
- Create: `internal/dashboard/ui/setup_templ.go`
- Create: `internal/dashboard/ui/setup_assets.go`
- Create: `internal/dashboard/ui/setup_ui_test.go`
- Create: `internal/dashboard/ui/assets/setup.css`
- Create: `internal/dashboard/ui/assets/setup.js`
- Create: `internal/dashboard/ui/setup_client_test.mjs`
- Delete: `internal/dashboard/ui/capabilities.templ`
- Delete: `internal/dashboard/ui/capabilities_templ.go`
- Delete: `internal/dashboard/ui/capabilities_assets.go`
- Delete: `internal/dashboard/ui/capabilities_ui_test.go`
- Delete: `internal/dashboard/ui/assets/capabilities.css`
- Delete: `internal/dashboard/ui/assets/capabilities.js`
- Delete: `internal/dashboard/ui/capabilities_client_test.mjs`
- Modify: `internal/dashboard/ui/navigation.templ`
- Modify: `internal/dashboard/ui/navigation_templ.go`
- Modify: `internal/dashboard/ui/layout.templ`
- Modify: `internal/dashboard/ui/layout_templ.go`
- Modify: `internal/dashboard/ui/assets.go`
- Modify: `internal/dashboard/ui/assets_test.go`
- Modify: `internal/dashboard/shell.go`
- Modify: `internal/dashboard/shell_test.go`
- Modify: `mise.toml`

**Interfaces:**
- Consumes: `GET /api/v1/desk/setup`, bootstrap request token, and the existing
  restart polling contract.
- Produces: a Setup landing list with stable DOM hooks
  `#setup-models`, `#setup-github`, `#setup-telegram`, and `#setup-skills`, plus
  a single `#setup-task` region.

- [ ] **Step 1: Write rendering and client tests for route compatibility and focus behavior**

Render `ShellView{ActiveSection: "capabilities"}` and assert:

```go
for _, want := range []string{
	`id="desk-setup"`,
	`id="setup-title"`,
	`>Setup</a>`,
	`id="setup-models"`,
	`id="setup-github"`,
	`id="setup-telegram"`,
	`id="setup-skills"`,
	`id="setup-task"`,
} {
	if !strings.Contains(rendered, want) {
		t.Errorf("Setup shell missing %s", want)
	}
}
```

Node tests must prove status text is derived from the snapshot, only the active
task disables while pending, opening moves focus to the task heading, Back
returns focus to the originating action, and secrets clear on task exit.

- [ ] **Step 2: Run UI tests and verify they fail**

Run:

```sh
go test ./internal/dashboard/ui ./internal/dashboard -run 'Test(Setup|DeskSection)' -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: FAIL because Setup templates and assets do not exist.

- [ ] **Step 3: Implement the Setup template and responsive status-list CSS**

Keep `ActiveSection == "capabilities"` internally. Change only the visible
navigation label and page title to Setup. The template contains the status
list, required/optional labels, one primary button per row, a hidden focused
task region, global snapshot failure status, and restart status.

Use real `<button>`, `<a>`, `<fieldset>`, `<legend>`, `<label>`, and heading
elements. At narrow widths the status list remains one column; do not recreate
the three-column Capabilities grid. Replace
`capabilities_client_test.mjs` with `setup_client_test.mjs` in
`mise.toml`'s `dashboard-client-test` task.

- [ ] **Step 4: Implement the shared Setup client state machine**

Use:

```js
const state = {
  requestToken: document.body.dataset.requestToken || "",
  processGeneration: "",
  snapshot: null,
  task: null,
  returnFocus: null,
  restarting: false,
};
```

`openTask(name, trigger)`, `closeTask()`, `loadSetup()`,
`postMutation(path, body)`, `runTaskMutation(action)`, and `pollRestart()` are
the only shared flow primitives. Error rendering accepts server `code` and
`message` but displays only `message`; page-level status is used only when the
whole snapshot cannot load.

- [ ] **Step 5: Regenerate templ output and run the UI suite**

Run:

```sh
mise run dashboard-generate
go test ./internal/dashboard/ui ./internal/dashboard -count=1
node --test internal/dashboard/ui/*_client_test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit the persistent Setup shell**

```sh
git add internal/dashboard/ui internal/dashboard/shell.go internal/dashboard/shell_test.go
git commit -m "feat: replace capabilities with setup"
```

---

### Task 4: Make provider catalogue rows selectable and add a model in one task

**Files:**
- Modify: `internal/dashboard/capabilities.go`
- Modify: `internal/dashboard/capabilities_test.go`
- Modify: `internal/dashboard/setup.go`
- Modify: `internal/dashboard/setup_test.go`
- Modify: `internal/dashboard/ui/setup.templ`
- Modify: `internal/dashboard/ui/setup_templ.go`
- Modify: `internal/dashboard/ui/assets/setup.js`
- Modify: `internal/dashboard/ui/assets/setup.css`
- Modify: `internal/dashboard/ui/setup_client_test.mjs`

**Interfaces:**
- Consumes: `modelcatalog.AliasFor`, `CapabilityCatalogueView`,
  `Capabilities.AddModel`, restart scheduling, and optional current
  `CapabilitySession`.
- Produces:

```go
type AddCatalogueModelRequest struct {
	ConnectionName string `json:"connection_name"`
	UpstreamModel  string `json:"upstream_model"`
	Alias          string `json:"alias"`
	Default        bool   `json:"default"`
	Utility        bool   `json:"utility"`
}

type AddCatalogueModelResult struct {
	Mutation providerconfig.MutationResult `json:"mutation"`
	Alias    string                        `json:"alias"`
}
```

- [ ] **Step 1: Write domain and HTTP tests for generated aliases and the combined mutation**

Test `modelcatalog.AliasFor("openai/gpt-5-mini") == "openai-gpt-5-mini"` through the
Setup service, explicit alias override, alias collision, default/utility roles,
session selection after successful restart, probe failure rollback, and stable
errors:

```go
tests := []struct {
	err  error
	code string
}{
	{providerconfig.ErrLocked, "setup_locked"},
	{providerconfig.ErrConnectionNotFound, "provider_not_found"},
	{modelcatalog.ErrRefreshFailed, "catalogue_refresh_failed"},
	{ErrCapabilityModelNotFound, "model_not_found"},
	{providerconfig.ErrAliasConflict, "model_alias_conflict"},
	{fmt.Errorf("%w: model alias", providerconfig.ErrProbeFailed), "model_probe_failed"},
}
```

Use typed sentinel errors from `providerconfig` for collision and probe
classification rather than matching arbitrary strings. Add
`modelcatalog.ErrRefreshFailed` at the catalogue HTTP boundary and wrap network,
authentication, parse, and upstream status failures without exposing the
upstream response body.

- [ ] **Step 2: Run model setup tests and verify they fail**

Run:

```sh
go test ./internal/dashboard ./internal/providerconfig -run 'Test(AddCatalogueModel|SetupModelError)' -count=1
node --test --test-name-pattern='model' internal/dashboard/ui/setup_client_test.mjs
```

Expected: FAIL because combined model enrollment and typed error mappings do
not exist.

- [ ] **Step 3: Add typed provider errors and the Setup model endpoint**

Export `providerconfig.ErrAliasConflict`, `ErrProbeFailed`,
`ErrConnectionNotFound`, and `ErrInvalidAlias`, wrapping them at their current
validation points. Add
`POST /api/v1/desk/setup/models` under existing mutation middleware. Treat the
upstream ID as untrusted input and call `AddModelWithMode(CommitForRestart)`;
the existing provider probe is the authoritative validation and preserves the
manual-entry fallback when discovery is unavailable.

The response is `202 Accepted`. Session assignment is offered only after the
new process returns; the client then calls the existing session-model endpoint
with the confirmed alias.

- [ ] **Step 4: Implement the focused Add model task**

The task renders enrolled connections as radios (preselecting the only item),
Refresh, search, a maximum of 50 visible result rows, and a result summary.
Each result is a real radio row. Selecting one fills:

```js
state.modelDraft = {
  connection_name: state.catalogue.connection,
  upstream_model: model.id,
  alias: aliasFor(model.id),
  default: !state.snapshot.providers.default_model,
  utility: false,
};
```

The server supplies the canonical generated alias alongside each sanitized
catalogue model so the browser does not duplicate Go slug rules. Alias and
role controls live under an Advanced disclosure. Label the role checkboxes
**Use for new conversations** and **Use for utility work**. Confirmation shows
provider, exact upstream ID, alias, and selected roles. The primary action is
**Add model**. After restart and canonical reload, show **Use in this
conversation** only when the Setup snapshot contains a current Desk session;
that action calls the existing session-model route.

- [ ] **Step 5: Run model domain, HTTP, and client tests**

Run:

```sh
go test ./internal/modelcatalog ./internal/providerconfig ./internal/dashboard -count=1
go test ./internal/dashboard/ui -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: PASS, including keyboard selection, 50-row render bound, focus,
restart polling, role assignment, and secret-free JSON.

- [ ] **Step 6: Commit selectable model enrollment**

```sh
git add internal/modelcatalog internal/providerconfig internal/dashboard
git commit -m "feat: add models from the setup catalogue"
```

---

### Task 5: Add provider presets to the focused Setup task

**Files:**
- Create: `internal/providerconfig/presets.go`
- Create: `internal/providerconfig/presets_test.go`
- Modify: `internal/dashboard/capabilities.go`
- Modify: `internal/dashboard/capabilities_test.go`
- Modify: `internal/dashboard/ui/setup.templ`
- Modify: `internal/dashboard/ui/setup_templ.go`
- Modify: `internal/dashboard/ui/assets/setup.js`
- Modify: `internal/dashboard/ui/setup_client_test.mjs`
- Modify: `cmd/waffle/provider_cmd.go`
- Modify: `cmd/waffle/provider_cmd_test.go`

**Interfaces:**
- Consumes: existing provider enrollment, catalogue refresh, and
  `providerAddGuided`.
- Produces:

```go
type Preset struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	ConnectionName string `json:"connection_name"`
	Type           string `json:"type"`
	BaseURL        string `json:"base_url,omitempty"`
	Credential     bool   `json:"credential"`
	BaseURLVisible bool   `json:"base_url_visible"`
}

func Presets() []Preset
func PresetByID(string) (Preset, bool)
```

The exact presets are OpenAI (`openai`, empty base URL), Anthropic
(`anthropic`), OpenRouter (`openrouter`,
`https://openrouter.ai/api/v1`), and OpenAI-compatible (custom connection and
base URL).

- [ ] **Step 1: Write preset and provider-flow tests**

Assert presets are sorted in the intended display order, contain no
credentials, and return defensive copies. HTTP tests cover preset
normalization, manual upstream fallback when discovery is unsupported,
credential clearing, collision, and transactional probe failure.

- [ ] **Step 2: Run tests and verify they fail**

Run:

```sh
go test ./internal/providerconfig ./internal/dashboard ./cmd/waffle -run 'Test(ProviderPreset|SetupProvider)' -count=1
```

Expected: FAIL because presets are not shared.

- [ ] **Step 3: Implement shared presets and use them in CLI and Desk**

Move existing guided CLI preset facts into `providerconfig.Presets`. Keep
terminal prompting in `cmd/waffle`; do not move I/O into the domain package.
Desk posts only the chosen normalized connection, credential, selected models,
and roles to the existing provider enrollment operation.

- [ ] **Step 4: Implement the Add provider task**

Use preset radios, prefilled connection name, password credential input,
per-value Base URL override, discovery, default selection, optional utility
selection, favourites, and a final summary. Manual upstream entry appears only
after a discovery-unsupported response. Clear the credential in `finally` and
when Back is chosen.

- [ ] **Step 5: Run provider, dashboard, CLI, and client tests**

Run:

```sh
go test ./internal/providerconfig ./internal/dashboard ./cmd/waffle -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit provider presets and guided enrollment**

```sh
git add internal/providerconfig internal/dashboard cmd/waffle/provider_cmd.go cmd/waffle/provider_cmd_test.go
git commit -m "feat: guide provider setup in desk"
```

---

### Task 6: Verify the first implementation slice

**Files:**
- Modify only if a verification failure requires a scoped fix.

**Interfaces:**
- Consumes: all deliverables from Tasks 1–5.
- Produces: a green, independently reviewable Setup/models slice.

- [ ] **Step 1: Run formatting and focused tests**

```sh
mise run fmt
go test ./internal/configtxn ./internal/providerconfig ./internal/modelcatalog ./internal/dashboard ./internal/dashboard/ui ./cmd/waffle -count=1
node --test internal/dashboard/ui/*_client_test.mjs
git diff --check
```

Expected: every command passes.

- [ ] **Step 2: Run the repository gates**

```sh
mise run dashboard-check
mise run vet
mise run lint
mise run test
mise run build
```

Expected: every command passes.

- [ ] **Step 3: Perform local Safari acceptance for the model flow**

Start a fixture Desk on loopback, open `/desk/?section=capabilities` in Safari,
and verify the visible label is Setup, statuses render, one provider is
preselected, catalogue rows are keyboard-selectable, Add model generates an
alias, the credential field is blank after submission/reload, restart polling
recovers, and the added alias can be selected for the current conversation.

- [ ] **Step 4: Commit any verification-only fixes**

If verification changed files:

```sh
git add internal/configtxn internal/providerconfig internal/modelcatalog internal/dashboard cmd/waffle
git commit -m "fix: harden setup model onboarding"
```

If no files changed, record the commands and Safari evidence in the execution
report without creating an empty commit.
