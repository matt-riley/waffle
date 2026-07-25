# Guided Dashboard Setup: GitHub Onboarding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator configure and verify repository-scoped GitHub access from Desk or `waffle setup github`, using a fine-grained PAT for the short path or a GitHub App for least privilege.

**Architecture:** Add a typed `internal/setupconfig` service over the shared `configtxn` engine. GitHub verification lives in `internal/gitcred` and accepts credentials only in memory; setup configuration validates first, then commits `github/token` or `[github.app]` plus `github/app-key` atomically. Desk and CLI become thin input adapters over the same methods.

**Tech Stack:** Go 1.25.12, `net/http`, existing `internal/gitcred` GitHub App client, shared `internal/configtxn`, encrypted age secret store, templ, embedded JavaScript.

## Global Constraints

- GitHub remains optional and never changes Waffle's Installed/Ready lifecycle.
- Fine-grained PAT is the shortest path; GitHub App is labeled **Recommended for least privilege**.
- Credentials are accepted once, validated before commit, never returned, logged, placed in TOML, stored in browser storage, or retained in the DOM.
- A PAT is stored only as `github/token`; an App private key is stored only as `github/app-key`.
- `[github.app].private_key` is always `secret://github/app-key`.
- Exact workspace repository binding remains the credential broker authorization boundary.
- When no current repository exists, report credential verification without claiming repository access.
- HTTP clients have fixed timeouts, bounded bodies, no credential-bearing redirects, and sanitized errors.
- Existing `waffle secret set github/token` and raw App config remain compatible recovery paths.
- Desk mutations retain request-token, idempotency, body-limit, CSP, Host, Origin, fetch-metadata, no-store, and restart behavior.

---

### Task 1: Add safe GitHub credential verification

**Files:**
- Create: `internal/gitcred/verify.go`
- Create: `internal/gitcred/verify_test.go`
- Modify: `internal/gitcred/app.go`
- Modify: `internal/gitcred/app_test.go`

**Interfaces:**
- Consumes: GitHub REST APIs and `App.Credential`.
- Produces:

```go
type Verification struct {
	Login              string
	Repository         string
	CredentialVerified bool
	RepositoryVerified bool
	ContentsWrite      bool
}

type Verifier interface {
	VerifyPAT(context.Context, token, repository string) (Verification, error)
	VerifyApp(context.Context, AppConfig, repository string) (Verification, error)
}

type AppConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	BaseURL        string
}

type HTTPVerifier struct {
	Client *http.Client
}
```

`repository` is empty or canonical lowercase-insensitive `owner/repo`.

- [ ] **Step 1: Write PAT and App verification tests**

Use `httptest.Server` and assert:

```text
PAT:
  GET /user
  GET /repos/{owner}/{repo} when repository is non-empty

App:
  POST /app/installations/{installation}/access_tokens
  GET /repos/{owner}/{repo} when repository is non-empty
```

Verify `Authorization: Bearer <credential>`, GitHub Accept/User-Agent headers,
repository JSON `permissions.push`, a 64 KiB response limit, ten-second default
timeout, context cancellation, redirect rejection when an Authorization header
would cross origin, and stable sentinels:

```go
var (
	ErrCredentialInvalid = errors.New("github credential invalid")
	ErrRepositoryDenied  = errors.New("github repository access denied")
	ErrContentsWriteMissing = errors.New("github contents write permission missing")
)
```

Assert tokens and PEM content never occur in returned errors.

- [ ] **Step 2: Run verifier tests and verify they fail**

```sh
go test ./internal/gitcred -run 'TestVerify' -count=1
```

Expected: FAIL because verification does not exist.

- [ ] **Step 3: Implement PAT verification**

Call `/user` first. If repository is empty, return
`CredentialVerified: true` and leave repository fields false. Otherwise call
`/repos/{owner}/{repo}`, require a 2xx response and `permissions.push == true`,
and return `ContentsWrite: true`. Map 401 to `ErrCredentialInvalid`; map
403/404 or missing push permission to the repository sentinels without
including response bodies.

- [ ] **Step 4: Implement App verification over existing minting**

Construct `gitcred.NewApp`, mint a repository-scoped token when a repository is
present, and verify that repository. When repository is empty, add an
installation validation method that mints an installation token without
narrowing to a repository:

```go
func (a *App) VerifyInstallation(context.Context) error
```

It calls the existing installation-token endpoint with contents-write
permission but no repository list and discards the returned token.

- [ ] **Step 5: Run the Git credential suite**

```sh
go test ./internal/gitcred -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit GitHub verification**

```sh
git add internal/gitcred
git commit -m "feat: verify github setup credentials"
```

---

### Task 2: Add transactional GitHub setup configuration

**Files:**
- Create: `internal/setupconfig/manager.go`
- Create: `internal/setupconfig/github.go`
- Create: `internal/setupconfig/github_test.go`
- Create: `internal/setupconfig/toml.go`
- Create: `internal/setupconfig/toml_test.go`
- Modify: `internal/configtxn/engine.go`
- Modify: `internal/configtxn/engine_test.go`

**Interfaces:**
- Consumes: `configtxn.Engine`, `gitcred.Verifier`, parsed Waffle config, and
  current workspace repository supplied by the caller.
- Produces:

```go
type GitHubMethod string

const (
	GitHubNone GitHubMethod = "none"
	GitHubPAT  GitHubMethod = "pat"
	GitHubApp  GitHubMethod = "app"
)

type GitHubStatus struct {
	Method             GitHubMethod `json:"method"`
	Configured         bool         `json:"configured"`
	Verified           bool         `json:"verified"`
	Repository         string       `json:"repository,omitempty"`
	RepositoryVerified bool         `json:"repository_verified"`
	Summary            string       `json:"summary"`
}

type GitHubPATRequest struct {
	Token      string
	Repository string
}

type GitHubAppRequest struct {
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	BaseURL        string
	Repository     string
}

type Manager struct {
	Txn      *configtxn.Engine
	GitHub   gitcred.Verifier
}

func (m *Manager) GitHubStatus(context.Context, string) (GitHubStatus, error)
func (m *Manager) ConfigureGitHubPAT(context.Context, GitHubPATRequest, configtxn.Mode) (GitHubStatus, configtxn.Result, error)
func (m *Manager) ConfigureGitHubApp(context.Context, GitHubAppRequest, configtxn.Mode) (GitHubStatus, configtxn.Result, error)
func (m *Manager) VerifyGitHub(context.Context, string) (GitHubStatus, error)
```

`GitHubStatus` performs a bounded live verification when a credential is
configured. With a current repository it verifies that exact repository;
without one it verifies only the credential/installation and leaves
`RepositoryVerified` false. A verification failure is returned as a typed
status error so Setup can mark only the optional GitHub row
`needs_attention`—it must not fail the whole snapshot.

- [ ] **Step 1: Write transaction and status tests**

Cover:

- missing secret/config reports `none`;
- `github/token` reports PAT configured but not verified until live verify;
- complete App config and key report App configured;
- incomplete App configuration fails closed;
- raw TOML private keys are rejected;
- validation runs before transaction staging;
- PAT success from none/PAT changes only `github/token`;
- App success changes `[github.app]` and `github/app-key`;
- switching methods removes the superseded secret/config only after new
  validation succeeds;
- every injected secret/config/lifecycle failure restores previous bytes and
  prior credential method;
- returned status and errors contain no token, PEM, secret reference, base URL
  credential, or raw TOML.

- [ ] **Step 2: Run setupconfig tests and verify they fail**

```sh
go test ./internal/setupconfig -run 'TestGitHub' -count=1
```

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement minimal canonical TOML mutation**

Move no provider-specific code. `toml.go` exposes only:

```go
func SetGitHubApp(raw []byte, app config.GitHubApp) ([]byte, error)
func ClearGitHubApp(raw []byte) ([]byte, error)
```

Preserve unrelated comments/tables using the same line-oriented canonical table
rules already proven by `providerconfig`; reject noncanonical duplicate or
quoted managed tables rather than rewriting unrelated config.

- [ ] **Step 4: Implement PAT and App configuration**

Validate the credential against the requested repository before calling
`Txn.Apply`. From none/PAT, PAT creates a secret-only plan. When replacing an
App, PAT also calls `ClearGitHubApp` and deletes `github/app-key` in the same
transaction:

```go
plan := configtxn.Plan{
	ConfigBytes: clearAppCandidate,
	SecretOps: []configtxn.SecretOp{
		{Name: "github/token", Value: req.Token},
		{Name: "github/app-key", Delete: true},
	},
}
```

App creates:

```go
plan := configtxn.Plan{
	ConfigBytes: candidate,
	SecretOps: []configtxn.SecretOp{
		{Name: "github/app-key", Value: string(req.PrivateKey)},
		{Name: "github/token", Delete: true},
	},
}
```

Clear request token/key byte slices on every return path. The successful
configuration status may retain verified login/repository in memory for the
response, but no durable config is added for those display facts.

- [ ] **Step 5: Run setup and transaction tests**

```sh
go test ./internal/configtxn ./internal/setupconfig ./internal/gitcred -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit transactional GitHub setup**

```sh
git add internal/setupconfig internal/configtxn
git commit -m "feat: configure github transactionally"
```

---

### Task 3: Expose sanitized GitHub Setup status and mutations

**Files:**
- Create: `internal/dashboard/setup_github.go`
- Create: `internal/dashboard/setup_github_test.go`
- Modify: `internal/dashboard/setup.go`
- Modify: `internal/dashboard/setup_test.go`
- Modify: `internal/dashboard/api.go`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/dashboard_wiring.go`
- Modify: `cmd/waffle/dashboard_wiring_test.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/serve_cmd_test.go`

**Interfaces:**
- Consumes: `setupconfig.Manager`, optional current workspace repository,
  restart scheduler, and mutation middleware.
- Produces:

```text
POST /api/v1/desk/setup/github/pat
POST /api/v1/desk/setup/github/app
POST /api/v1/desk/setup/github/verify
```

Request bodies:

```go
type githubPATBody struct {
	Token string `json:"token"`
}

type githubAppBody struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key"`
	BaseURL        string `json:"base_url"`
}
```

Repository is server-derived from the current Desk session/workspace; clients
cannot request verification against an arbitrary repository.

- [ ] **Step 1: Write snapshot and mutation HTTP tests**

Test all GitHub row states, strict JSON, 64 KiB body limit, request token,
idempotent replay, secret clearing at handler return, restart scheduling only
after commit, server-derived repository, and stable errors:

```go
var githubHTTPErrorCases = []struct {
	err    error
	status int
	code   string
}{
	{gitcred.ErrCredentialInvalid, http.StatusUnauthorized, "github_credential_invalid"},
	{gitcred.ErrRepositoryDenied, http.StatusForbidden, "github_repository_denied"},
	{gitcred.ErrContentsWriteMissing, http.StatusForbidden, "github_repository_denied"},
	{configtxn.ErrLocked, http.StatusConflict, "setup_locked"},
}
```

- [ ] **Step 2: Run HTTP tests and verify they fail**

```sh
go test ./internal/dashboard ./cmd/waffle -run 'TestSetupGitHub' -count=1
```

Expected: FAIL because GitHub routes are not wired.

- [ ] **Step 3: Implement GitHub route adapter and snapshot projection**

Handlers copy credential strings to byte slices, clear both body fields and
copies in `defer`, call `setupconfig` with `configtxn.AwaitRestart`, and return
only `GitHubStatus` plus generic mutation metadata. The Setup row action is
`connect`, `verify`, or `repair` based on live status; the row is always
`required: false`. Give snapshot verification its own bounded context and map a
verification failure to row state `needs_attention` rather than returning a
page-level Setup error.

- [ ] **Step 4: Wire setupconfig into the serve process**

Construct one shared `configtxn.Engine`; pass it to both provider configuration
and `setupconfig.Manager`. Supply the current repository through a narrow
resolver based on the active Desk session's workspace binding. Call the shared
engine's `FinalizeDeferred` exactly once during startup, replacing the
provider-only finalization call.

- [ ] **Step 5: Run dashboard and serve tests**

```sh
go test ./internal/dashboard ./internal/setupconfig ./cmd/waffle -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit GitHub Setup HTTP wiring**

```sh
git add internal/dashboard cmd/waffle
git commit -m "feat: expose github setup in desk"
```

---

### Task 4: Build the GitHub Desk task

**Files:**
- Modify: `internal/dashboard/ui/setup.templ`
- Modify: `internal/dashboard/ui/setup_templ.go`
- Modify: `internal/dashboard/ui/assets/setup.js`
- Modify: `internal/dashboard/ui/assets/setup.css`
- Modify: `internal/dashboard/ui/setup_ui_test.go`
- Modify: `internal/dashboard/ui/setup_client_test.mjs`

**Interfaces:**
- Consumes: GitHub Setup row/status and routes from Task 3.
- Produces: one method chooser with PAT and recommended App subflows.

- [ ] **Step 1: Write client tests for PAT, App, and verification states**

Assert:

- method selection is explicit;
- GitHub App is labeled recommended;
- PAT copy requests repository contents read/write and repository restriction;
- App fields are App ID, Installation ID, PEM private key, and advanced Base URL;
- the status never claims repository verification when none was available;
- both token and PEM fields clear after success, failure, Back, and reload;
- non-secret IDs survive validation errors;
- safe server errors render inline at the credential field;
- successful restart polling reloads canonical status; and
- no credential is present in serialized state, URL, DOM text, or storage.

- [ ] **Step 2: Run UI tests and verify they fail**

```sh
go test ./internal/dashboard/ui -run 'TestSetupGitHub' -count=1
node --test --test-name-pattern='github' internal/dashboard/ui/setup_client_test.mjs
```

Expected: FAIL because the GitHub task does not exist.

- [ ] **Step 3: Implement the method chooser and PAT task**

Use a password input with `autocomplete="off"`, a concise permissions
explanation, repository scope summary when one is present, and one **Connect
GitHub** submit action. Always clear the input in `finally`.

- [ ] **Step 4: Implement the recommended App task and verify/repair actions**

Use numeric inputs with minimum 1 for IDs and a multiline `<textarea>` for the
private key with `autocomplete="off"`, `spellcheck="false"`, Safari
`-webkit-text-security: disc`, and an explicit reveal toggle. Never repopulate
it. Put Base URL inside Advanced. Configured rows show method and verification
state but never secret references. **Verify** performs a credential-free POST;
**Repair** returns to the method chooser.

- [ ] **Step 5: Run UI and route tests**

```sh
go test ./internal/dashboard/ui ./internal/dashboard ./internal/setupconfig -count=1
node --test internal/dashboard/ui/setup_client_test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit the GitHub Desk task**

```sh
git add internal/dashboard/ui
git commit -m "feat: guide github connection setup"
```

---

### Task 5: Extend `waffle setup` with equivalent GitHub onboarding

**Files:**
- Create: `cmd/waffle/setup_github.go`
- Create: `cmd/waffle/setup_github_test.go`
- Modify: `cmd/waffle/setup_cmd.go`
- Modify: `cmd/waffle/setup_cmd_test.go`
- Modify: `cmd/waffle/main.go`
- Modify: `cmd/waffle/completion_cmd.go`

**Interfaces:**
- Consumes: `setupconfig.Manager`, existing hidden/piped secret reader, and
  current repository discovery.
- Produces:

```text
waffle setup github
```

The command prompts for PAT or App, reads the credential through the existing
hidden terminal/piped path, and calls the same domain methods with
`configtxn.Reconcile`.

- [ ] **Step 1: Write CLI tests**

Test help, unknown option, PAT success, App success, invalid credential,
repository denied, no repository available, configured rerun, switching
methods, cancellation, piped secrets, terminal-secret seam, and output
redaction. Assert neither a token nor PEM appears in stdout, stderr, error
trees, argv, config, or test telemetry.

- [ ] **Step 2: Run CLI tests and verify they fail**

```sh
go test ./cmd/waffle -run 'TestSetupGitHub' -count=1
```

Expected: FAIL because the subcommand does not exist.

- [ ] **Step 3: Route `waffle setup github` without breaking first-run setup**

Keep bare `waffle setup` behavior, then print optional next actions for GitHub
and Telegram. Dispatch `setup github` before the current unknown-option branch.
Use dependency variables in tests exactly as provider setup does; do not create
a second setupconfig implementation.

- [ ] **Step 4: Implement interactive prompts and output**

Prompt for method, IDs, optional Base URL, and credential. Success output is:

```text
GitHub credential verified.
Repository owner/repo: verified with contents write access.
```

or, without a repository:

```text
Credential verified; repository access will be checked when a workspace opens.
```

- [ ] **Step 5: Run CLI and setup domain tests**

```sh
go test ./cmd/waffle ./internal/setupconfig ./internal/gitcred -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit CLI GitHub setup**

```sh
git add cmd/waffle
git commit -m "feat: guide github setup in cli"
```

---

### Task 6: Document and verify GitHub onboarding

**Files:**
- Modify: `docs/waffle-desk.md`
- Modify: `docs/deploy.md`
- Modify: `docs/chat.md`
- Modify: `config.example.toml`
- Modify: corresponding documentation tests if present.

**Interfaces:**
- Consumes: completed Desk and CLI GitHub flows.
- Produces: operator instructions that lead with Setup, then CLI recovery.

- [ ] **Step 1: Update documentation**

Document the Desk path, PAT repository selection and contents read/write
permission, recommended App fields, exact workspace broker scope, status
meanings, verification without a current repository, and recovery:

```sh
waffle setup github
waffle secret set github/token
```

Keep raw `[github.app]` configuration in the advanced/recovery section.

- [ ] **Step 2: Run focused and repository gates**

```sh
mise run fmt
go test ./internal/gitcred ./internal/configtxn ./internal/setupconfig ./internal/dashboard ./internal/dashboard/ui ./cmd/waffle -count=1
node --test internal/dashboard/ui/*_client_test.mjs
mise run dashboard-check
mise run vet
mise run lint
mise run test
mise run build
git diff --check
```

Expected: every command passes.

- [ ] **Step 3: Perform Safari and workspace acceptance**

Through managed Desk in Safari, configure a repository-limited fine-grained
PAT, verify the exact current repository, reload and prove the token field is
blank, open a workspace for that repository, and exercise `git fetch` plus a
write-capable credential request through the existing broker. Repeat the
configuration verification with a GitHub App fixture or live test
installation. Verify a different repository cannot obtain the credential.

- [ ] **Step 4: Commit documentation or verification fixes**

```sh
git add docs/waffle-desk.md docs/deploy.md docs/chat.md config.example.toml internal/gitcred internal/configtxn internal/setupconfig internal/dashboard cmd/waffle
git commit -m "docs: guide github onboarding"
```
