# Waffle Desk Capabilities and Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete Waffle Desk with session skills, global model and skill management, reviewed skill installation, connection visibility, real-browser QA, and release-ready documentation.

**Architecture:** Add one migration for session-skill attachments and install provenance, then expose capability services through the established secure dashboard boundary. Provider transactions and skill audit remain authoritative; the final task adds Playwright development tests without adding a runtime dependency to the released binary.

**Tech Stack:** Go 1.25.12, `templ` v0.3.1020, SQLite, existing provider/model catalogue services, audited filesystem operations, `git` subprocess with fixed arguments, pnpm, Playwright

## Global Constraints

- Complete the foundation/Today and operations plans before this plan.
- Session model choices use existing `sessions.model_alias`; global default changes never rewrite existing sessions.
- Session skill attachment forces an active skill into that session context but does not deactivate, install, or exclude other active skills.
- New skills install inactive and require a second explicit activation action.
- Accept only local directories below configured import roots or HTTPS Git URLs on configured hosts with a full commit hash.
- Reject symlinks, special files, path traversal, name collisions, over-limit trees, invalid `SKILL.md`, and unreviewed content.
- Never return, cache, persist in browser storage, or log provider credentials.
- Provider/model removal and skill update/uninstall remain out of scope.
- Keep the dashboard disabled by default for this release.
- Use test-first steps, preserve unrelated files, and use Conventional Commits.

---

### Task 1: Add session-skill and install-provenance persistence

**Files:**
- Create: `internal/store/migrations/0024_dashboard_capabilities.sql`
- Modify: `internal/store/store_test.go`
- Create: `internal/skill/attachments.go`
- Create: `internal/skill/attachments_test.go`
- Modify: `internal/skill/learn.go`
- Modify: `internal/skill/learn_test.go`

**Interfaces:**
- Consumes: sessions and existing `skill_status`
- Produces: `skill.Attachments`, install `source_ref` and `content_digest`

- [ ] **Step 1: Write failing migration and attachment tests**

```go
func TestAttachmentsAreUniqueAndCascadeWithSession(t *testing.T) {
	st := openTestStore(t)
	sessions := session.New(st)
	sess, _ := sessions.Create(context.Background(), "dashboard")
	a := &Attachments{DB: st.DB}
	if err := a.Attach(context.Background(), sess.ID, "github-review"); err != nil {
		t.Fatal(err)
	}
	if err := a.Attach(context.Background(), sess.ID, "github-review"); err != nil {
		t.Fatal(err)
	}
	got, _ := a.List(context.Background(), sess.ID)
	if !slices.Equal([]string{"github-review"}, got) {
		t.Fatalf("attachments = %v", got)
	}
}
```

Also test detach, missing session, deterministic order, migration columns, and
status upsert preserving provenance during activation.

- [ ] **Step 2: Run tests and confirm missing schema**

Run: `go test ./internal/store ./internal/skill -run 'Test(Attachments|SkillStatus|Migrations)' -count=1`

Expected: FAIL because migration 24 and `Attachments` do not exist.

- [ ] **Step 3: Add schema and store**

```sql
CREATE TABLE session_skills (
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    skill_name   TEXT NOT NULL,
    attached_at  TEXT NOT NULL,
    PRIMARY KEY (session_id, skill_name)
) STRICT;

ALTER TABLE skill_status ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_status ADD COLUMN content_digest TEXT NOT NULL DEFAULT '';
```

```go
type Attachments struct { DB *sql.DB }
func (a *Attachments) Attach(ctx context.Context, sessionID, name string) error
func (a *Attachments) Detach(ctx context.Context, sessionID, name string) error
func (a *Attachments) List(ctx context.Context, sessionID string) ([]string, error)
```

Extend status persistence with a typed `StatusRecord` so activation changes
status/activated time without erasing source reference or digest.

- [ ] **Step 4: Run migration and race tests**

Run: `go test -race ./internal/store ./internal/skill -run 'Test(Attachments|SkillStatus|Migrations)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations/0024_dashboard_capabilities.sql internal/store/store_test.go internal/skill
git commit -m "feat: persist dashboard session skills"
```

### Task 2: Apply attached skills in the shared chat runtime

**Files:**
- Modify: `internal/chat/commands.go`
- Modify: `internal/chat/commands_test.go`
- Modify: `internal/chat/types.go`
- Modify: `cmd/waffle/chat_runtime.go`
- Modify: `cmd/waffle/chat_runtime_test.go`
- Modify: `internal/chatwire/frame.go`
- Modify: `internal/chatwire/codec_test.go`

**Interfaces:**
- Consumes: `skill.Attachments`, active skill library, existing chat commands
- Produces: `/skills`, `/skills attach <name>`, `/skills detach <name>`, `chat.State.AttachedSkills`

- [ ] **Step 1: Write failing command/runtime tests**

Test list, attach active skill, reject inactive/missing skill, idempotent attach,
detach without deactivation, resume persistence, removed-skill actionable state,
and socket/dashboard JSON compatibility.

```go
func TestAttachedSkillIsForcedIntoSessionSystemContext(t *testing.T) {
	runtime := newRuntimeWithSkill(t, "reviewer", "Review every changed file.")
	openRuntime(t, runtime)
	commandRuntime(t, runtime, "/skills attach reviewer")
	runTurn(t, runtime, "check this")
	if !strings.Contains(runtime.agent.System, "<attached_skill name=\"reviewer\">") {
		t.Fatal("attached skill body missing from system context")
	}
}
```

- [ ] **Step 2: Run tests and confirm missing command**

Run: `go test ./internal/chat ./internal/chatwire ./cmd/waffle -run 'Test(AttachedSkill|SkillsCommand|ChatEventJSON)' -count=1`

Expected: FAIL because the command and state fields do not exist.

- [ ] **Step 3: Implement exact command behavior**

```go
type SkillRef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Attached    bool   `json:"attached"`
	Missing     bool   `json:"missing"`
}

// Add to chat.State:
Skills []SkillRef `json:"skills"`
```

Add `CommandSkills`. Runtime open loads attachments; attach/detach persists then
rebuilds the skill block from active `Skill.Body()` values:

```text
<attached_skills>
<attached_skill name="reviewer">
Review every changed file.
</attached_skill>
</attached_skills>
```

Keep the agent's base system prompt separately so repeated changes replace, not
append, this block. Other active skills remain in normal metadata discovery.

- [ ] **Step 4: Run shared-chat tests**

Run: `go test -race ./internal/chat ./internal/chatwire ./cmd/waffle -run 'Test(AttachedSkill|SkillsCommand|ChatEventJSON|SessionOwner)' -count=1`

Expected: PASS across direct and socket backends.

- [ ] **Step 5: Commit**

```bash
git add internal/chat internal/chatwire cmd/waffle/chat_runtime.go cmd/waffle/chat_runtime_test.go
git commit -m "feat: add session skill attachments"
```

### Task 3: Build the reviewed skill installer

**Files:**
- Create: `internal/skillinstall/installer.go`
- Create: `internal/skillinstall/manifest.go`
- Create: `internal/skillinstall/source.go`
- Create: `internal/skillinstall/audit.go`
- Create: `internal/skillinstall/installer_test.go`
- Create: `internal/skillinstall/testdata/valid/SKILL.md`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.toml`

**Interfaces:**
- Consumes: configured import roots/Git hosts, skills root, `skill.Discover`, skill audit/status
- Produces: `Installer.Stage`, `Installer.Install`, review manifest and single-use stage ID

- [ ] **Step 1: Write the security test matrix**

Table-test local root escape, relative traversal, symlink file/dir, FIFO,
duplicate name, missing/invalid `SKILL.md`, more than 64 files, more than 1 MiB,
Git HTTP URL, disallowed host, missing/short commit, fetch ref mismatch, audit
failure, stage expiry, digest mismatch, atomic rename failure, and cleanup.

```go
type StageRequest struct {
	LocalPath string
	GitURL   string
	Commit   string
}

func TestStageRejectsUnpinnedGit(t *testing.T) {
	installer := newInstaller(t)
	_, err := installer.Stage(context.Background(), StageRequest{GitURL: "https://github.com/acme/skill.git", Commit: "main"})
	if !errors.Is(err, ErrCommitRequired) {
		t.Fatalf("error = %v", err)
	}
}
```

- [ ] **Step 2: Run tests and confirm missing package**

Run: `go test ./internal/skillinstall -count=1`

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement bounded staging and review**

```go
type Manifest struct {
	Name          string      `json:"name"`
	Description   string      `json:"description"`
	SourceRef     string      `json:"source_ref"`
	ContentDigest string      `json:"content_digest"`
	Files         []FileEntry `json:"files"`
	Audit         AuditView   `json:"audit"`
	StageID       string      `json:"stage_id"`
	ExpiresAt     time.Time   `json:"expires_at"`
}

type FileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Preview string `json:"preview,omitempty"`
}

type AuditView struct {
	Passed bool     `json:"passed"`
	Flags  []string `json:"flags"`
}

type GitFetcher interface {
	Fetch(context.Context, gitURL, commit, destination string) error
}

type Installer struct {
	SkillsRoot  string
	StageRoot   string
	ImportRoots []string
	GitHosts    []string
	Fetcher     GitFetcher
	Now         func() time.Time
	Random      io.Reader
}

func (i *Installer) Stage(context.Context, StageRequest) (Manifest, error)
func (i *Installer) Install(context.Context, stageID, digest string) (skill.Skill, error)
```

Use `filepath.EvalSymlinks` only to validate allowed local source roots, then
walk with `Lstat` and reject every symlink/special file. Git fetch uses
`exec.CommandContext` with fixed arguments, `--no-checkout`, and the exact
40-character lowercase hex commit; no shell. Copy to a `0700` private stage,
write regular files `0600`, compute a sorted path/content SHA-256 manifest,
audit, and expire after 10 minutes. Install verifies the digest again, injects
`status: inactive`, fsyncs, and atomically renames into a non-existing skill
directory.

`audit.go` performs a deterministic structural review over the staged manifest:
it requires one parseable `SKILL.md` with matching slug/name and a one-line
description; rejects binaries, hidden VCS state, executable bits, absolute-path
entries, and NUL bytes; and reports shell/code/network-reference files as
explicit review flags. A local source persists only `local:<directory-name>`,
never the source path. A Git source persists the credential-free HTTPS URL plus
`@<commit>`.
Each UTF-8 text file includes its complete bounded content in `Preview`; the UI
escapes it and renders a unified all-additions diff. Binary files are rejected,
so review never contains an opaque attachment.

Add:

```go
type Dashboard struct {
	Enabled          bool     `toml:"enabled"`
	SkillImportRoots []string `toml:"skill_import_roots"`
	SkillGitHosts    []string `toml:"skill_git_hosts"`
}
```

Defaults are empty; example configuration lists `github.com` only as a
commented operator choice.

- [ ] **Step 4: Run security and race tests**

Run: `go test -race ./internal/skillinstall ./internal/config -count=1`

Expected: PASS; tests execute no real network requests.

- [ ] **Step 5: Commit**

```bash
git add internal/skillinstall internal/config config.example.toml
git commit -m "feat: add reviewed skill installer"
```

### Task 4: Expose Models, Skills, and provider enrollment

**Files:**
- Modify: `internal/providerconfig/manager.go`
- Modify: `internal/providerconfig/manager_test.go`
- Create: `internal/dashboard/restart.go`
- Create: `internal/dashboard/restart_test.go`
- Create: `internal/dashboard/capabilities.go`
- Create: `internal/dashboard/capabilities_test.go`
- Create: `internal/dashboard/ui/capabilities.templ`
- Create: `internal/dashboard/ui/assets/capabilities.js`
- Modify: `internal/dashboard/ui/today.templ`
- Modify: `internal/dashboard/ui/assets/today.js`
- Modify: `internal/dashboard/ui/assets/app.css`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Generate: `internal/dashboard/ui/*_templ.go`

**Interfaces:**
- Consumes: provider manager/catalogue, skill library/status/attachments, installer, service restart scheduler
- Produces: typed provider snapshot, deferred restart transaction, utility-role mutation, Capabilities APIs and UI

- [ ] **Step 1: Write failing transaction and secret-leak tests**

Test typed listing order, default and utility role changes, probe rollback,
catalogue refresh, add alias, provider enrollment rollback, secret absence from
JSON/logs/errors, session model isolation, skill attach/detach, stage/install,
inactive-by-default, explicit activation, deferred-restart journal recovery,
and response-before-restart ordering.

```go
func TestProviderEnrollmentResponseNeverContainsCredential(t *testing.T) {
	const secret = "sk-super-private"
	rec := postProvider(t, newCapabilitiesHandler(t), secret)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("credential leaked in response")
	}
}
```

- [ ] **Step 2: Run tests and confirm missing typed operations**

Run: `go test ./internal/providerconfig ./internal/dashboard -run 'Test(Provider|Capabilities|Model|Skill)' -count=1`

Expected: FAIL because `Snapshot`, utility activation, and handlers do not exist.

- [ ] **Step 3: Extract typed provider operations and add APIs**

```go
func (m *Manager) Snapshot(ctx context.Context) (Listing, error)
func (m *Manager) ActivateUtilityModel(ctx context.Context, alias string) error

type CommitMode int
const (
	CommitAndReconcile CommitMode = iota
	CommitForRestart
)

type MutationResult struct {
	RestartRequired bool
	TransactionID   string
}

func (m *Manager) AddWithMode(context.Context, AddRequest, CommitMode) (MutationResult, error)
func (m *Manager) AddModelWithMode(context.Context, AddModelRequest, CommitMode) (MutationResult, error)
func (m *Manager) ActivateModelWithMode(context.Context, string, CommitMode) (MutationResult, error)
func (m *Manager) ActivateUtilityModelWithMode(context.Context, string, CommitMode) (MutationResult, error)
func (m *Manager) FinalizeDeferred(context.Context) error
```

Keep `List` as deterministic JSON over `Snapshot`. Utility activation mirrors
default activation's lock, probe, staging, rollback, and unrelated-config
checks. Add `UtilityModel string` to `providerconfig.Listing` so the UI never
infers the role from model aliases.

Existing CLI methods delegate to `CommitAndReconcile` and preserve current
restart/health/rollback behavior. Dashboard mutations use `CommitForRestart`,
which validates, probes, atomically commits config/secrets, and leaves the
existing recovery journal in an `awaiting_restart` phase without attempting to
restart the process that is serving the request.

```go
type RestartScheduler interface {
	Schedule(context.Context, string) error
}
```

The handler returns `202` with `restart_required=true`, flushes the response,
then schedules restart. Managed mode runs fixed arguments
`systemctl --no-block restart waffle.service`; standalone mode returns a
sanitized instruction to restart `waffle serve` and does not exit. On managed
startup, `serveWaffle` calls `FinalizeDeferred` after `/healthz` is serving; a
healthy new process finalizes the journal, while failure follows the existing
recovery/rollback rules. No restart command contains user input.

Expose:

```text
GET  /api/v1/desk/capabilities
POST /api/v1/desk/models/session
POST /api/v1/desk/models/default
POST /api/v1/desk/models/utility
POST /api/v1/desk/models/catalogue/refresh
POST /api/v1/desk/models
POST /api/v1/desk/providers
POST /api/v1/desk/skills/session/attach
POST /api/v1/desk/skills/session/detach
POST /api/v1/desk/skills/stage
POST /api/v1/desk/skills/install
POST /api/v1/desk/skills/{name}/activate
```

Provider request bodies are capped at 64 KiB, credentials are copied into the
transaction request then zeroed where practical, and request bodies are never
logged. Model/skill removal routes do not exist.

- [ ] **Step 4: Render and verify Capabilities**

Implement Models, Skills, and Tools & connections tabs; explicit Session versus
Waffle-wide labels; catalogue search; credential form with
`autocomplete="off"`; reviewed install manifest/diff; inactive install; separate
activation; Today model/skill controls; immediate credential-field clearing on
both success and failure; and a restarting/disconnected state that polls
bootstrap without replaying the mutation.

Run:

```bash
mise run dashboard-generate
go test -race ./internal/providerconfig ./internal/skill ./internal/skillinstall ./internal/dashboard ./cmd/waffle -run 'Test(Provider|Capabilities|Model|Skill)' -count=1
```

Expected: PASS with no credential in captured responses or logs.

- [ ] **Step 5: Commit**

```bash
git add internal/providerconfig internal/dashboard cmd/waffle/serve_cmd.go
git commit -m "feat: add Waffle Desk capabilities"
```

### Task 5: Add sanitized Tools and connections visibility

**Files:**
- Create: `internal/dashboard/connections.go`
- Create: `internal/dashboard/connections_test.go`
- Modify: `internal/dashboard/ui/capabilities.templ`
- Modify: `internal/dashboard/ui/assets/capabilities.js`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Generate: `internal/dashboard/ui/*_templ.go`

**Interfaces:**
- Consumes: configured adapter labels, MCP names, sandbox/profile policy, health
- Produces: credential-free `ConnectionView` records

- [ ] **Step 1: Write failing redaction tests**

Seed config with secret references, API-key-shaped values, private environment
names, local paths, and raw MCP commands. Assert none appear in JSON; only name,
kind, status, profile, sandbox mode, egress summary, and sanitized guidance are
allowed.

- [ ] **Step 2: Implement the allowlisted view**

```go
type ConnectionView struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Profile     string `json:"profile,omitempty"`
	SandboxMode string `json:"sandbox_mode,omitempty"`
	Egress      string `json:"egress,omitempty"`
	Guidance    string `json:"guidance,omitempty"`
}
```

Build records from explicit fields rather than marshaling config structs.
Expose read-only `GET /api/v1/desk/connections`; arbitrary MCP editing and
environment display remain absent.

- [ ] **Step 3: Render and verify**

Run:

```bash
mise run dashboard-generate
go test ./internal/dashboard -run TestConnections -count=1
```

Expected: PASS with stable empty arrays and no private values.

- [ ] **Step 4: Commit**

```bash
git add internal/dashboard
git commit -m "feat: show dashboard connection health"
```

### Task 6: Add real-browser, responsive, accessibility, and rollout gates

**Files:**
- Create: `tools/dashboard-tests/package.json`
- Create: `tools/dashboard-tests/pnpm-lock.yaml`
- Create: `tools/dashboard-tests/playwright.config.mjs`
- Create: `tools/dashboard-tests/tests/desk.spec.mjs`
- Create: `tools/dashboard-tests/fixtures/fake-server.go`
- Modify: `mise.toml`
- Modify: `docs/deploy.md`
- Modify: `docs/usage-guide.md`
- Modify: `README.md`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: complete embedded dashboard and deterministic fake services
- Produces: `mise run dashboard-check`, documented access/rollout, browser evidence for all five sections

- [ ] **Step 1: Add the failing browser suite**

Use `@playwright/test` exactly `1.61.1` in `package.json` and pin its resolved
graph in `pnpm-lock.yaml`. Configure Playwright with `channel: "chrome"` so the
test uses the system Chrome and does not download a browser during normal test
runs. Cover:

```json
{
  "name": "waffle-dashboard-tests",
  "private": true,
  "scripts": {"test": "playwright test"},
  "devDependencies": {"@playwright/test": "1.61.1"}
}
```

```js
test("session model does not change global default", async ({ page }) => {
  await page.goto("/desk/");
  await page.getByLabel("Session model").selectOption("local");
  await expect(page.getByLabel("Session model")).toHaveValue("local");
  await page.getByRole("link", { name: "Capabilities" }).click();
  await expect(page.getByLabel("Default model")).toHaveValue("primary");
});
```

Add flows for Today send/cancel; task filter/open-at-desk; workspace
open/idle/resume/select/close preview; memory search/attach/cancel forget/confirm
forget; skill stage/review/install/activate; provider enrollment redaction; SSE
disconnect; keyboard-only navigation; dialog focus return; reduced motion; and
no page-level overflow at 1470, 768, 375, and 320 pixels plus 200% zoom.

- [ ] **Step 2: Run once and confirm the new suite is red**

Run: `pnpm --dir tools/dashboard-tests test`

Expected: FAIL until the fake server and all selectors/contracts are complete.

- [ ] **Step 3: Implement the deterministic harness and repository task**

The Go fake server must use the real embedded handler with in-memory fakes,
listen on `127.0.0.1:0`, print only its URL, and make no network calls.

Add:

```toml
[tools]
node = "26.1.0"
pnpm = "11.9.0"

[tasks.dashboard-install]
run = "pnpm --dir tools/dashboard-tests install --frozen-lockfile"

[tasks.dashboard-test]
run = "mise run dashboard-install && pnpm --dir tools/dashboard-tests test"

[tasks.dashboard-check]
run = """
go tool templ generate -path internal/dashboard/ui
git diff --exit-code -- internal/dashboard/ui/*_templ.go
go test -race ./internal/dashboard ./internal/skillinstall
pnpm --dir tools/dashboard-tests test
"""
```

Include `mise run dashboard-check` in `mise run test`.

Add a `dashboard-browser` job to `.github/workflows/ci.yml` that installs the
pinned Node/pnpm toolchain through mise, runs
`pnpm --dir tools/dashboard-tests install --frozen-lockfile`, and then runs
`mise run dashboard-check`. Keep the reusable Go jobs unchanged.

- [ ] **Step 4: Complete documentation**

Document one-binary delivery, disabled-by-default enablement, local URL,
SSH/Tailscale SSH port forwarding, no public bind/reverse proxy, model/skill
scope rules, installer allowlists, forget exclusions, credential handling, and
rollback by disabling `[dashboard] enabled`.

- [ ] **Step 5: Run the complete release gate**

Run:

```bash
pnpm --dir tools/dashboard-tests install --frozen-lockfile
mise run dashboard-check
mise run fmt
mise run vet
mise run lint
mise run test
mise run build
git diff --check
```

Expected: all Go and browser tests PASS, no generated-code diff, no lint/build
failure, and no horizontal overflow or accessibility violation in the required
browser matrix.

- [ ] **Step 6: Commit**

```bash
git add tools/dashboard-tests mise.toml docs/deploy.md docs/usage-guide.md README.md internal/dashboard/ui .github/workflows/ci.yml
git commit -m "test: verify Waffle Desk end to end"
```
