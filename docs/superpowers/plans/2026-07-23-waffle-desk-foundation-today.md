# Waffle Desk Foundation and Today Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a secure, disabled-by-default `templ` cockpit inside the Waffle binary with the approved shell, live status, and a fully working Today conversation.

**Architecture:** `waffle serve` composes the existing observability routes with a new `internal/dashboard` handler on the loopback admin listener. Server-rendered `templ` components and embedded assets provide the UI; a bounded server-sent-event hub and per-browser chat backend provide live conversation without creating another control plane.

**Tech Stack:** Go 1.25.12, `templ` v0.3.1020, `net/http`, server-sent events, semantic HTML, CSS, browser ES modules, SQLite-backed existing chat runtime

## Global Constraints

- Serve the HTML shell at `/desk/` and operational endpoints below `/api/v1/desk`.
- Keep `gateway.status_listen` loopback-only; default address remains `127.0.0.1:8422`.
- Preserve the existing `/healthz` and `/status` response contracts.
- Keep `[dashboard] enabled = false` for existing and new installations in this plan.
- Ship generated Go and all static assets inside the Waffle binary; no Node runtime, CDN, external font, third-party script, analytics, or adjacent asset directory.
- Use `templ` components with typed view models; components never query stores or perform mutations.
- Emit no permissive CORS headers; reject invalid Host, Origin, and fetch-metadata values.
- Never log prompts, transcript bodies, tool payloads, credentials, memory excerpts, raw configuration, or unsanitized error chains.
- Use test-first steps, preserve all unrelated untracked files, and use Conventional Commits.

---

### Task 1: Pin `templ` and add the dashboard configuration gate

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `mise.toml`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.toml`

**Interfaces:**
- Consumes: existing Go 1.25.12 toolchain and strict TOML decoding
- Produces: `config.Dashboard{Enabled bool}`, `go tool templ`, and `mise run dashboard-generate`

- [ ] **Step 1: Write failing configuration tests**

```go
func TestDashboardDefaultsDisabled(t *testing.T) {
	if Default().Dashboard.Enabled {
		t.Fatal("dashboard must default disabled")
	}
}

func TestDashboardEnabledLoads(t *testing.T) {
	path := writeConfig(t, "[dashboard]\nenabled = true\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.Enabled {
		t.Fatal("dashboard.enabled = false, want true")
	}
}
```

- [ ] **Step 2: Run the tests and confirm the missing field**

Run: `go test ./internal/config -run 'TestDashboard(Default|Enabled)' -count=1`

Expected: FAIL because `Config.Dashboard` does not exist.

- [ ] **Step 3: Add the typed setting and pinned generator**

```go
type Dashboard struct {
	Enabled bool `toml:"enabled"`
}
```

Add this field to the root `config.Config` struct:

```go
Dashboard Dashboard `toml:"dashboard"`
```

Run:

```bash
go get -tool github.com/a-h/templ/cmd/templ@v0.3.1020
```

Add to `mise.toml`:

```toml
[tasks.dashboard-generate]
description = "Generate Waffle Desk templ components"
run = "go tool templ generate -path internal/dashboard/ui"
```

Add the disabled example to `config.example.toml`:

```toml
[dashboard]
enabled = false
```

- [ ] **Step 4: Verify config and tool resolution**

Run:

```bash
go test ./internal/config -run 'TestDashboard(Default|Enabled)' -count=1
go tool templ version
```

Expected: both tests PASS and the tool prints `v0.3.1020`.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum mise.toml internal/config/config.go internal/config/config_test.go config.example.toml
git commit -m "build: add Waffle Desk templ toolchain"
```

### Task 2: Compose a secure admin handler

**Files:**
- Create: `internal/dashboard/security.go`
- Create: `internal/dashboard/security_test.go`
- Create: `internal/dashboard/idempotency.go`
- Create: `internal/dashboard/idempotency_test.go`
- Create: `internal/dashboard/router.go`
- Create: `internal/dashboard/router_test.go`
- Modify: `internal/observability/http.go`
- Modify: `internal/observability/observability_test.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/serve_cmd_test.go`

**Interfaces:**
- Consumes: `gateway.status_listen`, `observability.Service`, `http.Handler`
- Produces: `dashboard.NewSecurity(listen string, random io.Reader)`, `(*Security).Wrap(http.Handler) http.Handler`, `IdempotencyStore.Do`, `observability.RegisterRoutes(*http.ServeMux, *Service)`

- [ ] **Step 1: Write failing security and compatibility tests**

```go
func TestSecurityRejectsCrossSiteMutation(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", nil)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	security.Wrap(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
```

Also assert: same-origin GET succeeds; invalid Host fails; `Sec-Fetch-Site: cross-site` fails; POST without `X-Waffle-Desk-Token` fails; responses include CSP, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `X-Frame-Options: DENY`; `/healthz` and `/status` JSON remain byte-shape compatible.

Add idempotency tests proving an identical endpoint/body/key returns the first
canonical response without re-running its callback, while reuse against a
different endpoint or body returns `409 idempotency_conflict`.

- [ ] **Step 2: Run the focused tests and confirm missing packages**

Run: `go test ./internal/dashboard ./internal/observability ./cmd/waffle -run 'Test(Security|Status|Health|ServeDashboard)' -count=1`

Expected: FAIL because `internal/dashboard` and route composition do not exist.

- [ ] **Step 3: Implement the security boundary and route registration**

```go
type Security struct {
	token        string
	allowedHosts map[string]struct{}
}

func NewSecurity(listen string, random io.Reader) (*Security, error)
func (s *Security) Token() string
func (s *Security) Wrap(next http.Handler) http.Handler
func (s *Security) RequireMutation(next http.Handler) http.Handler

func RegisterRoutes(mux *http.ServeMux, service *observability.Service)

type idempotencyEntry struct {
	operation string
	digest    string
	status    int
	body      []byte
	expiresAt time.Time
	ready     chan struct{}
}

type IdempotencyStore struct {
	mu       sync.Mutex
	now      func() time.Time
	capacity int
	ttl      time.Duration
	entries  map[string]*idempotencyEntry
}
func NewIdempotencyStore(now func() time.Time, capacity int, ttl time.Duration) *IdempotencyStore
func (s *IdempotencyStore) Do(
	ctx context.Context,
	key, operation, requestDigest string,
	run func(context.Context) (status int, body []byte),
) (status int, body []byte, err error)
```

`NewSecurity` must accept the configured loopback host:port plus `localhost` on
the same port, read 32 random bytes, and encode them with raw URL-safe base64.
`Wrap` must validate Host, same-origin Origin when present, and fetch metadata,
then apply the required headers. `RequireMutation` must use
`subtle.ConstantTimeCompare` on `X-Waffle-Desk-Token`.
It also requires `Idempotency-Key`. The router calculates a SHA-256 digest of
the bounded request body and calls a 512-entry, 10-minute `IdempotencyStore`
before invoking any mutation handler. Concurrent requests with the same key
join the first result; only completed sanitized status/body pairs are cached.

Refactor `observability.NewHandler` to create a mux and call
`observability.RegisterRoutes`, so existing tests and callers retain a stable
constructor. Add:

```go
func ServeHandler(ctx context.Context, listener net.Listener, handler http.Handler) error
```

Keep `ServeListener(ctx, listener, service)` as a compatibility wrapper over
`ServeHandler(..., NewHandler(service))`. In `serveWaffle`, build one mux,
register observability first, register dashboard routes only when
`cfg.Dashboard.Enabled`, wrap it with dashboard security, and pass it to
`observability.ServeHandler`.

- [ ] **Step 4: Run security, observability, and race tests**

Run:

```bash
go test -race ./internal/dashboard ./internal/observability ./cmd/waffle -run 'Test(Security|Status|Health|ServeDashboard)' -count=1
```

Expected: PASS with no race report; disabled configuration returns 404 for `/desk/`.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/security.go internal/dashboard/security_test.go internal/dashboard/idempotency.go internal/dashboard/idempotency_test.go internal/dashboard/router.go internal/dashboard/router_test.go internal/observability/http.go internal/observability/observability_test.go cmd/waffle/serve_cmd.go cmd/waffle/serve_cmd_test.go
git commit -m "feat: add secure dashboard route boundary"
```

### Task 3: Build the approved `templ` shell and embed its assets

**Files:**
- Create: `internal/dashboard/ui/layout.templ`
- Create: `internal/dashboard/ui/navigation.templ`
- Create: `internal/dashboard/ui/today.templ`
- Create: `internal/dashboard/ui/viewmodel.go`
- Create: `internal/dashboard/ui/assets.go`
- Create: `internal/dashboard/ui/assets/app.css`
- Create: `internal/dashboard/ui/assets/app.js`
- Generate: `internal/dashboard/ui/*_templ.go`
- Create: `internal/dashboard/shell.go`
- Create: `internal/dashboard/shell_test.go`

**Interfaces:**
- Consumes: `Security.Token()`, typed shell state, embedded assets
- Produces: `ui.Shell(ui.ShellView) templ.Component`, `dashboard.ShellHandler`

- [ ] **Step 1: Write failing shell tests**

```go
func TestShellRendersApprovedFiveSectionNavigation(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))
	body := rec.Body.String()
	for _, text := range []string{"Today", "Tasks", "Workspaces", "Memory", "Capabilities"} {
		if !strings.Contains(body, ">"+text+"<") {
			t.Errorf("missing navigation %q", text)
		}
	}
	if strings.Contains(body, "https://") {
		t.Fatal("shell must not load external assets")
	}
}
```

Also test HTML escaping, `lang="en"`, skip link, focusable native controls,
`data-request-token`, hashed local asset URLs, and `Cache-Control: no-store`.

- [ ] **Step 2: Run the shell test and confirm it fails**

Run: `go test ./internal/dashboard -run TestShell -count=1`

Expected: FAIL because `ShellHandler` and the components do not exist.

- [ ] **Step 3: Implement the shell and visual tokens**

```go
type ShellView struct {
	Title          string
	ActiveSection  string
	Connection     string
	ModelAlias     string
	RequestToken   string
	AssetVersion   string
}
```

Use these root tokens in `app.css`:

```css
:root {
  --cream: #f4eddf;
  --paper: #fffaf0;
  --ink: #211d19;
  --muted: #746b61;
  --ginger: #dd7128;
  --healthy: #2f7d55;
  --danger: #b83a32;
  --rule: #d7c9b7;
  --shadow: 3px 3px 0 #211d19;
}
```

Implement the approved desktop rail, bottom mobile navigation below 768px,
conversation column, task-context column, visible focus, 320px no-overflow
rules, and reduced-motion media query. Embed the asset directory with
`//go:embed assets/*`, calculate a SHA-256 version at startup, and serve assets
with immutable caching while the shell remains `no-store`.

- [ ] **Step 4: Generate and verify**

Run:

```bash
mise run dashboard-generate
go test ./internal/dashboard ./internal/dashboard/ui -run TestShell -count=1
git diff --exit-code -- internal/dashboard/ui/*_templ.go
```

Expected: tests PASS and a second generation produces no diff.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/ui internal/dashboard/shell.go internal/dashboard/shell_test.go
git commit -m "feat: add embedded Waffle Desk shell"
```

### Task 4: Add bounded bootstrap state and server-sent events

**Files:**
- Create: `internal/dashboard/api.go`
- Create: `internal/dashboard/bootstrap.go`
- Create: `internal/dashboard/bootstrap_test.go`
- Create: `internal/dashboard/events.go`
- Create: `internal/dashboard/events_test.go`
- Modify: `internal/dashboard/router.go`

**Interfaces:**
- Consumes: observability snapshot, server clock, security token
- Produces: `Bootstrap`, `Event`, `EventHub.Publish`, `EventHub.Subscribe`, `GET /api/v1/desk/bootstrap`, `GET /api/v1/desk/events`

- [ ] **Step 1: Write failing event and bootstrap tests**

```go
func TestEventHubRequiresResyncAfterCursorFallsBehind(t *testing.T) {
	hub := NewEventHub(2)
	hub.Publish(Event{Type: "one"})
	hub.Publish(Event{Type: "two"})
	hub.Publish(Event{Type: "three"})
	_, resync := hub.Subscribe(0)
	if !resync {
		t.Fatal("old cursor must require resync")
	}
}
```

Also assert strictly increasing cursors, slow-subscriber eviction, context
cancellation, SSE `id/event/data` framing, `Last-Event-ID`, heartbeat comments,
and bootstrap arrays serialized as `[]`, never `null`.

- [ ] **Step 2: Run tests and confirm missing event types**

Run: `go test -race ./internal/dashboard -run 'Test(EventHub|Bootstrap|Events)' -count=1`

Expected: FAIL because the hub and endpoints do not exist.

- [ ] **Step 3: Implement exact event contracts**

```go
type Bootstrap struct {
	Version      string                 `json:"version"`
	ServerTime   time.Time              `json:"server_time"`
	RequestToken string                 `json:"request_token"`
	EventCursor  uint64                 `json:"event_cursor"`
	Health       observability.Health   `json:"health"`
	Status       observability.Snapshot `json:"status"`
}

type Event struct {
	Cursor     uint64          `json:"cursor"`
	Type       string          `json:"type"`
	Resource  string          `json:"resource,omitempty"`
	ResourceID string          `json:"resource_id,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type EventHub struct {
	mu          sync.Mutex
	capacity    int
	next        uint64
	ring        []Event
	subscribers map[chan Event]struct{}
}
func NewEventHub(capacity int) *EventHub
func (h *EventHub) Publish(Event) Event
func (h *EventHub) Subscribe(after uint64) (<-chan Event, bool)
func (h *EventHub) Unsubscribe(<-chan Event)
```

Use a 256-event ring and subscriber buffers of 32. Never block publishers.
When a cursor is unavailable, send `event: resync_required` and close.

- [ ] **Step 4: Run the race tests**

Run: `go test -race ./internal/dashboard -run 'Test(EventHub|Bootstrap|Events)' -count=1`

Expected: PASS with no race report.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/api.go internal/dashboard/bootstrap.go internal/dashboard/bootstrap_test.go internal/dashboard/events.go internal/dashboard/events_test.go internal/dashboard/router.go
git commit -m "feat: add dashboard bootstrap and event stream"
```

### Task 5: Bridge browser clients to the existing chat runtime

**Files:**
- Create: `internal/dashboard/chat.go`
- Create: `internal/dashboard/chat_test.go`
- Modify: `internal/dashboard/router.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/serve_cmd_test.go`

**Interfaces:**
- Consumes: `func(context.Context) (chat.Backend, error)`, existing `chat.Backend`, shared `chatSessionOwners`
- Produces: `ChatClients.Open`, `ChatClients.Turn`, `ChatClients.Command`, `ChatClients.Cancel`, `ChatClients.Close`

- [ ] **Step 1: Write failing lifecycle tests with a fake backend**

```go
func TestChatClientDoesNotRetryTurnAfterDisconnect(t *testing.T) {
	backend := &fakeBackend{}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, newDeterministicIDs())
	client, _, err := clients.Open(context.Background(), chat.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := clients.Turn(context.Background(), client, "ship it"); err != nil {
		t.Fatal(err)
	}
	clients.Close(context.Background(), client)
	if backend.turnCalls != 1 {
		t.Fatalf("turn calls = %d, want 1", backend.turnCalls)
	}
}
```

Also test unknown client, one active turn, cancel, close cleanup, 30-minute idle
expiry, 64-client cap, event sanitization, session-active conflict, and service
shutdown.

- [ ] **Step 2: Run tests and confirm missing client manager**

Run: `go test -race ./internal/dashboard -run TestChat -count=1`

Expected: FAIL because `ChatClients` does not exist.

- [ ] **Step 3: Implement the bounded manager and HTTP endpoints**

```go
type BackendFactory func(context.Context) (chat.Backend, error)

type chatClient struct {
	backend    chat.Backend
	lastActive time.Time
	busy       bool
}

type ChatClients struct {
	mu         sync.Mutex
	clients    map[string]*chatClient
	factory    BackendFactory
	ids        io.Reader
	now        func() time.Time
	maxClients int
	idleTTL    time.Duration
	events     *EventHub
}
func NewChatClients(factory BackendFactory, ids io.Reader) *ChatClients
func (c *ChatClients) Open(context.Context, chat.OpenOptions) (clientID string, state chat.State, err error)
func (c *ChatClients) Turn(context.Context, string, string) error
func (c *ChatClients) Command(context.Context, string, chat.ParsedCommand) (chat.Result, error)
func (c *ChatClients) Cancel(string) error
func (c *ChatClients) Close(context.Context, string) error
func (c *ChatClients) Shutdown(context.Context) error
```

Expose mutation-token-protected endpoints:

```text
POST /api/v1/desk/chat/open
POST /api/v1/desk/chat/turn
POST /api/v1/desk/chat/command
POST /api/v1/desk/chat/cancel
POST /api/v1/desk/chat/close
```

Every request has a 64 KiB body limit and an idempotency key. Chat events are
published to the shared hub with `resource=chat` and the opaque client ID.
`serveWaffle` must create one `chatSessionOwners` value and inject it into both
the socket and dashboard runtime factories.

- [ ] **Step 4: Verify dashboard and socket ownership together**

Run:

```bash
go test -race ./internal/dashboard ./cmd/waffle -run 'Test(Chat|ServeDashboard|SessionOwner)' -count=1
```

Expected: PASS; opening the same session through socket and dashboard yields
the stable `session_active` error and never creates a second owner.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard/chat.go internal/dashboard/chat_test.go internal/dashboard/router.go cmd/waffle/serve_cmd.go cmd/waffle/serve_cmd_test.go
git commit -m "feat: connect Waffle Desk to chat runtime"
```

### Task 6: Finish the Today interaction and document local access

**Files:**
- Modify: `internal/dashboard/ui/today.templ`
- Modify: `internal/dashboard/ui/assets/app.js`
- Create: `internal/dashboard/ui/assets/today.js`
- Modify: `internal/dashboard/ui/assets/app.css`
- Modify: `internal/dashboard/shell_test.go`
- Modify: `internal/dashboard/chat_test.go`
- Modify: `docs/deploy.md`
- Modify: `docs/chat.md`
- Modify: `mise.toml`

**Interfaces:**
- Consumes: shell, bootstrap, SSE, chat endpoints, existing `/model` backend command
- Produces: usable Today conversation with resume, send, cancel, streaming, tool activity, and session-only model selection

- [ ] **Step 1: Add failing response and static-contract tests**

Assert the rendered Today view contains the session title, transcript region,
tool activity region, multiline composer, send/cancel controls, model select,
connection/profile/workspace labels, and quick-action links. Assert scripts use
`textContent`, never `innerHTML`, never local/session storage, and never retry a
turn automatically.

Run: `go test ./internal/dashboard -run 'Test(Shell|Chat|Today)' -count=1`

Expected: FAIL on the missing controls and module behavior.

- [ ] **Step 2: Implement the Today state machine**

Use these explicit states in `today.js`:

```js
const phase = Object.freeze({
  opening: "opening",
  idle: "idle",
  sending: "sending",
  streaming: "streaming",
  cancelling: "cancelling",
  disconnected: "disconnected",
});
```

On load: fetch bootstrap, open/resume the selected session, start SSE, and
render canonical state. On submit: disable send, POST exactly once with a new
idempotency key, then append deltas with text nodes. On cancel: POST cancel and
wait for `turn_done`. On disconnect: retain transcript, show a stale banner,
disable mutations, and require a bootstrap refresh. Use the existing backend
`/model <alias>` command for the session model picker.

- [ ] **Step 3: Add documentation and generation verification**

Document:

```text
[dashboard]
enabled = true

waffle serve
open http://127.0.0.1:8422/desk/
```

Document remote access as an explicit local port forward, not a public bind.
Extend `mise run test` to run `mise run dashboard-generate` followed by
`git diff --exit-code -- internal/dashboard/ui/*_templ.go`.

- [ ] **Step 4: Run the plan gate**

Run:

```bash
mise run dashboard-generate
go test -race ./internal/dashboard ./internal/observability ./cmd/waffle
mise run fmt
mise run vet
mise run build
git diff --exit-code -- internal/dashboard/ui/*_templ.go
```

Expected: every command exits 0; the binary contains `/desk/` assets and the
existing status/health tests remain green.

- [ ] **Step 5: Commit**

```bash
git add internal/dashboard docs/deploy.md docs/chat.md mise.toml
git commit -m "feat: complete Waffle Desk Today"
```
