# Waffle Desk Tasks, Workspaces, and Memory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add authoritative Tasks, Workspaces, and Memory sections to the secure Waffle Desk foundation.

**Architecture:** A typed dashboard application service aggregates existing observability, schedule, workspace, session, notes, workset, and usage services. Read endpoints return canonical view models; mutations reuse domain guards and a shared short-lived preview-token store for destructive operations.

**Tech Stack:** Go 1.25.12, existing SQLite stores, `templ`, JSON APIs, server-sent events, embedded ES modules

## Global Constraints

- Complete the foundation/Today plan before this plan.
- Do not add a generic task table; Tasks is a view over runs, schedules, retries, sessions, and deterministic attention rules.
- Do not issue SQL or edit configuration files from HTTP handlers.
- Reuse `workspace.Manager` lifecycle guards and never expose force-close in the first release.
- Memory results must include source, source ID, timestamp, excerpt, and available provenance.
- Forget must require preview plus a short-lived single-use confirmation token and must state provider-log, delivered-message, and backup exclusions.
- Browser state is never authoritative; every successful mutation returns canonical server state and publishes an event.
- Use test-first steps, preserve unrelated files, and use Conventional Commits.

---

### Task 1: Introduce the operations service and preview-token store

**Files:**
- Create: `internal/dashboard/operations.go`
- Create: `internal/dashboard/operations_test.go`
- Create: `internal/dashboard/previews.go`
- Create: `internal/dashboard/previews_test.go`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/serve_cmd.go`

**Interfaces:**
- Consumes: narrow interfaces implemented by observability, schedule, workspace, session, memory, workset, and usage stores
- Produces: `Operations`, `PreviewStore.Issue`, `PreviewStore.Consume`

- [ ] **Step 1: Write failing partial-failure and preview tests**

```go
func TestPreviewTokenIsSingleUseAndResourceBound(t *testing.T) {
	store := NewPreviewStore(fixedClock(), deterministicReader())
	token := store.Issue("workspace-close", "ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-2"); !errors.Is(err, ErrPreviewMismatch) {
		t.Fatalf("wrong resource error = %v", err)
	}
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("mismatched token replay error = %v", err)
	}
	token = store.Issue("workspace-close", "ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("replay error = %v", err)
	}
}
```

Also test expiry, bounded cleanup, concurrent consume, and an operations
snapshot that returns healthy sections plus a per-section sanitized error when
one dependency fails.

- [ ] **Step 2: Run tests and confirm missing service**

Run: `go test -race ./internal/dashboard -run 'Test(Preview|Operations)' -count=1`

Expected: FAIL because the types do not exist.

- [ ] **Step 3: Implement narrow interfaces and typed section errors**

```go
type RunReader interface { Snapshot(context.Context) (observability.Snapshot, error) }
type JobStore interface { List(context.Context) ([]schedule.Job, error) }
type SessionStore interface {
	Get(context.Context, string) (*session.Session, error)
	Search(context.Context, string, int) ([]session.Hit, error)
	SearchSummaries(context.Context, string, int) ([]session.Hit, error)
}
type NotesSearcher interface {
	Search(context.Context, string, int) ([]memory.NoteHit, error)
}
type WorksetStore interface {
	Add(context.Context, string, string, string, string, bool) (*workset.Entry, error)
}
type UsageReader interface {
	List(context.Context, string) ([]usage.Row, error)
}
type WorkspaceManager interface {
	List(context.Context) ([]workspace.Workspace, error)
	Get(context.Context, string) (*workspace.Workspace, error)
	OpenWithProfile(context.Context, string, string) (*workspace.Workspace, *sandbox.Client, error)
	Idle(context.Context, string) error
	Resume(context.Context, string) (*workspace.Workspace, *sandbox.Client, error)
	InspectClose(context.Context, string) (*workspace.CloseReport, error)
	Close(context.Context, string, bool) (*workspace.CloseReport, error)
}

type Operations struct {
	Runs RunReader
	Jobs JobStore
	Workspaces WorkspaceManager
	Sessions SessionStore
	Notes NotesSearcher
	Workset WorksetStore
	Usage UsageReader
	Previews *PreviewStore
	Events *EventHub
	Now func() time.Time
}
```

Implement `PreviewStore` with a mutex, 128-entry cap, 32 random bytes per token,
operation/resource binding, and deletion on first consume whether successful or
mismatched.

- [ ] **Step 4: Run race tests**

Run: `go test -race ./internal/dashboard -run 'Test(Preview|Operations)' -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/operations.go internal/dashboard/operations_test.go internal/dashboard/previews.go internal/dashboard/previews_test.go internal/dashboard/router.go cmd/waffle/serve_cmd.go
git commit -m "feat: add dashboard operations service"
```

### Task 2: Implement Tasks reads and schedule mutations

**Files:**
- Modify: `internal/schedule/schedule.go`
- Modify: `internal/schedule/schedule_test.go`
- Create: `internal/dashboard/tasks.go`
- Create: `internal/dashboard/tasks_test.go`
- Create: `internal/dashboard/ui/tasks.templ`
- Create: `internal/dashboard/ui/assets/tasks.js`
- Modify: `internal/dashboard/ui/navigation.templ`
- Modify: `internal/dashboard/ui/assets/app.css`
- Modify: `internal/dashboard/router.go`
- Generate: `internal/dashboard/ui/*_templ.go`

**Interfaces:**
- Consumes: run snapshot, schedule store, sessions, usage
- Produces: `schedule.Update`, `schedule.Store.Update`, `dashboard.TaskView`, Tasks read/mutation endpoints

- [ ] **Step 1: Write failing schedule and attention tests**

```go
func TestUpdateValidatesBeforeWriting(t *testing.T) {
	store := newTestStore(t)
	job := addJob(t, store)
	_, err := store.Update(context.Background(), job.ID, Update{Cron: "not cron"})
	if err == nil {
		t.Fatal("invalid cron update succeeded")
	}
	got, _ := store.Get(context.Background(), job.ID)
	if got.Cron != job.Cron {
		t.Fatal("failed update mutated stored job")
	}
}
```

Add table tests proving attention rules: failed/stalled run, exhausted retry,
disabled job with failure, and no false attention for successful history.

- [ ] **Step 2: Run focused tests and confirm missing update/view**

Run: `go test ./internal/schedule ./internal/dashboard -run 'Test(Update|Tasks|Attention)' -count=1`

Expected: FAIL because `schedule.Update` and Tasks views do not exist.

- [ ] **Step 3: Implement validated schedule update and Tasks API**

```go
type Update struct {
	Name    string
	Cron    string
	Prompt  string
	Deliver string
	Profile string
	Enabled bool
}

func (s *Store) Update(ctx context.Context, id string, in Update) (*Job, error)
```

Validate cron, non-empty name/prompt, profile format, and delivery syntax before
one SQL update. Expose:

```text
GET  /api/v1/desk/tasks?filter=all|active|scheduled|completed|attention
POST /api/v1/desk/tasks/schedules
POST /api/v1/desk/tasks/schedules/{id}
```

Return stable IDs, source, phase/profile, session, elapsed/runtime, usage,
outcome, retry state, evidence label, and `open_at_desk` only when a persisted
session exists.

- [ ] **Step 4: Render and verify Tasks**

Implement active/recent/schedule cards, four filters, attention summary, evidence
details, and Open at desk handoff. Run:

```bash
mise run dashboard-generate
go test -race ./internal/schedule ./internal/dashboard -run 'Test(Update|Tasks|Attention)' -count=1
```

Expected: PASS and generated code is stable.

- [ ] **Step 5: Commit**

```bash
git add internal/schedule internal/dashboard
git commit -m "feat: add Waffle Desk tasks"
```

### Task 3: Implement guarded workspace lifecycle

**Files:**
- Create: `internal/dashboard/workspaces.go`
- Create: `internal/dashboard/workspaces_test.go`
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`
- Create: `internal/dashboard/ui/workspaces.templ`
- Create: `internal/dashboard/ui/assets/workspaces.js`
- Modify: `internal/dashboard/ui/assets/app.css`
- Modify: `internal/dashboard/router.go`
- Generate: `internal/dashboard/ui/*_templ.go`

**Interfaces:**
- Consumes: `workspace.Manager`, current Today session, preview tokens
- Produces: `workspace.Manager.InspectClose`, workspace list/open/select/idle/resume/close-preview/close-confirm endpoints

- [ ] **Step 1: Write failing lifecycle tests**

Test valid `owner/repo`, invalid host paths, profile forwarding, open-client
cleanup, idle/resume, session selection, close preview evidence, preview-token
resource binding, dirty/unpushed refusal, and the absence of a force flag.

Add workspace package tests proving `InspectClose` gathers dirty/unpushed
evidence without removing the container or volume, restores an originally idle
workspace, and that `Close(ctx, id, false)` reuses the same inspection path.

```go
func TestWorkspaceCloseNeverForces(t *testing.T) {
	manager := &fakeWorkspaceManager{closeReport: &workspace.CloseReport{Dirty: " M file.go"}}
	handler := newWorkspaceHandler(t, manager)
	preview := postJSON(t, handler, "/api/v1/desk/workspaces/ws-1/close-preview", nil)
	postJSONExpect(t, handler, "/api/v1/desk/workspaces/ws-1/close", map[string]string{"preview_token": preview.Token}, http.StatusConflict)
	if manager.force {
		t.Fatal("dashboard requested force close")
	}
}
```

- [ ] **Step 2: Run tests and confirm missing handlers**

Run: `go test ./internal/dashboard -run TestWorkspace -count=1`

Expected: FAIL because workspace dashboard handlers do not exist.

- [ ] **Step 3: Implement exact endpoints and canonical views**

```text
GET  /api/v1/desk/workspaces
POST /api/v1/desk/workspaces/open
POST /api/v1/desk/workspaces/{id}/select
POST /api/v1/desk/workspaces/{id}/idle
POST /api/v1/desk/workspaces/{id}/resume
POST /api/v1/desk/workspaces/{id}/close-preview
POST /api/v1/desk/workspaces/{id}/close
```

Extract the current safety-check portion of `workspace.Manager.Close` into:

```go
func (m *Manager) InspectClose(ctx context.Context, id string) (*CloseReport, error)
```

The view includes ID, repository, session, status, profile, image, and resolved
egress label. Close preview calls `InspectClose`; it never calls `Close`.
Confirmation always calls `Close(ctx, id, false)`, which invokes the same
inspection again to defend against state changes between preview and confirm.
The preview has a 60-second token and includes dirty/unpushed evidence.

- [ ] **Step 4: Render and verify Workspaces**

Implement state-derived controls, open-repository dialog, profile and egress
labels, inline errors, and Today handoff. Run:

```bash
mise run dashboard-generate
go test -race ./internal/dashboard ./internal/workspace -run TestWorkspace -count=1
```

Expected: PASS with no force-close path.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat: add Waffle Desk workspaces"
```

### Task 4: Implement attributed memory search, attach, and forget

**Files:**
- Create: `internal/dashboard/memory.go`
- Create: `internal/dashboard/memory_test.go`
- Create: `internal/dashboard/ui/memory.templ`
- Create: `internal/dashboard/ui/assets/memory.js`
- Modify: `internal/dashboard/ui/assets/app.css`
- Modify: `internal/dashboard/router.go`
- Generate: `internal/dashboard/ui/*_templ.go`

**Interfaces:**
- Consumes: session search, summary search, `memory.NotesIndex`, `memory.Workspace`, `workset.Store`, preview tokens
- Produces: merged `MemoryHit`, attach endpoint, forget preview/confirm

- [ ] **Step 1: Write failing provenance and forget tests**

```go
func TestMemorySearchKeepsSourceAndStableID(t *testing.T) {
	service := newMemoryService(t)
	hits, err := service.Search(context.Background(), "security", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.Source == "" || hit.SourceID == "" || hit.Excerpt == "" {
			t.Fatalf("incomplete hit: %#v", hit)
		}
	}
}
```

Also test deterministic merge order, 20-result cap, archived labeling, workset
byte/count limits, exact forget preview exclusions, token expiry/replay, wrong
note binding, and no fake Undo response.

- [ ] **Step 2: Run tests and confirm missing memory service**

Run: `go test ./internal/dashboard -run TestMemory -count=1`

Expected: FAIL because `MemoryHit` and handlers do not exist.

- [ ] **Step 3: Implement the memory contract**

```go
type MemoryHit struct {
	Source     string    `json:"source"`
	SourceID   string    `json:"source_id"`
	Excerpt    string    `json:"excerpt"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
	Archived   bool      `json:"archived"`
	Provenance string    `json:"provenance,omitempty"`
}
```

Expose:

```text
GET  /api/v1/desk/memory?query=...
POST /api/v1/desk/memory/attach
POST /api/v1/desk/memory/{noteID}/forget-preview
POST /api/v1/desk/memory/{noteID}/forget
```

Attach adds a pinned `workset.KindFact` with `workset.SourceUser` and a bounded
source label plus excerpt. Forget preview must say it affects Waffle-owned
memory only and excludes provider logs, delivered messages, and backups.
Confirm consumes the 60-second token before calling `Workspace.ForgetNote`.

- [ ] **Step 4: Render and verify Memory**

Implement debounced search, source chips, attach-to-session, add-via-conversation
handoff, and a native confirmation dialog whose pre-confirm action is Cancel.
Run:

```bash
mise run dashboard-generate
go test -race ./internal/dashboard ./internal/memory ./internal/workset -run 'Test(Memory|Forget|Workset)' -count=1
```

Expected: PASS; confirmed deletion disappears only after canonical refresh.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard
git commit -m "feat: add Waffle Desk memory"
```

### Task 5: Verify the complete operations slice

**Files:**
- Modify: `docs/deploy.md`
- Modify: `docs/usage-guide.md`
- Modify: `internal/dashboard/router_test.go`
- Modify: `internal/dashboard/shell_test.go`

**Interfaces:**
- Consumes: complete Tasks, Workspaces, and Memory sections
- Produces: documented operations behavior and cross-section regression evidence

- [ ] **Step 1: Add cross-section tests**

Test the full sequence: failed run appears in Attention; Open at desk selects
its session; workspace open/select/idle/resume preserves that session; memory
search attaches a bounded fact to the same session; canceling forget changes
nothing; confirming forget removes only the selected note.

- [ ] **Step 2: Document exact behavior and exclusions**

Document filters, schedule validation, workspace safety, memory sources,
attach-to-session limits, and forget exclusions. Do not describe force-close,
public access, or provider-side deletion.

- [ ] **Step 3: Run the operations gate**

Run:

```bash
mise run dashboard-generate
go test -race ./internal/dashboard ./internal/schedule ./internal/workspace ./internal/memory ./internal/workset
mise run fmt
mise run vet
mise run build
git diff --exit-code -- internal/dashboard/ui/*_templ.go
```

Expected: every command exits 0.

- [ ] **Step 4: Commit**

```bash
git add internal/dashboard docs/deploy.md docs/usage-guide.md
git commit -m "docs: document Waffle Desk operations"
```
