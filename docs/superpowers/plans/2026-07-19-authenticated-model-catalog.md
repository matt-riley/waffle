# Authenticated Model Catalogue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator enroll OpenAI, Anthropic, OpenRouter, or a custom OpenAI-compatible provider by entering a credential and choosing models from a secure, cached authenticated catalogue.

**Architecture:** Provider-specific catalogue clients translate remote model-list responses into a small provider-neutral descriptor. A secure 24-hour on-disk cache serves already-enrolled connections, while the existing provider transaction manager remains authoritative for credentials, favourite aliases, service state, and rollback. Waffle owns discovery and selection; Infra only classifies the new model-add command as a lifecycle mutation; shared CI remains unchanged.

**Tech Stack:** Go 1.25.12, `net/http`, Anthropic Go SDK v1.58.0, TOML provider configuration, encrypted `age` secrets, Unix advisory file locks, Go `testing`/`httptest`, Ruby Minitest for Infra.

## Global Constraints

- Work from the approved design in `docs/superpowers/specs/2026-07-19-authenticated-model-catalog-design.md`.
- Preserve the existing `[providers]`, `[models]`, and `[agent]` persisted schema; no migration is permitted.
- Keep `llm.Provider` unchanged; model listing is an optional capability beside completion.
- OpenAI defaults to `https://api.openai.com/v1`, Anthropic to `https://api.anthropic.com`, and OpenRouter to `https://openrouter.ai/api/v1`; only custom OpenAI-compatible enrollment requires a URL.
- Classify OpenRouter after reload only when the normalized URL host is exactly `openrouter.ai` or ends in `.openrouter.ai`.
- Catalogue requests time out after 10 seconds; response bodies are capped at 16 MiB, catalogues at 10,000 models, strings at 4 KiB, and Anthropic pagination at 100 pages.
- Cache TTL is exactly 24 hours; cache directories are `0700`, files and lock files are `0600`, and cache data never contains credentials or credential fingerprints.
- Every enrollment stores a random opaque catalogue-scope ID in the encrypted secret store; cache applicability includes that ID so records cannot cross provider accounts.
- Reject provider-supplied model fields containing Unicode control characters before caching or terminal rendering.
- New enrollment always refreshes with the newly entered credential and never reads an old cache.
- Interactive enrollment must select at least one favourite even when no default or utility role is assigned.
- Explicit `--model ALIAS=UPSTREAM` enrollment performs no discovery request.
- Cache write or invalidation failure after a committed provider mutation is a warning and successful exit, never a false transaction failure.
- Preserve preflight-before-secret input, secret redaction, deny-by-default aliases, transactional probes, service rollback, and existing automation.
- Do not change `matt-riley-ci`.

---

## File structure

### Waffle

- Create `internal/modelcatalog/catalog.go`: normalized descriptor, search, and alias derivation.
- Create `internal/modelcatalog/catalog_test.go`: descriptor, search, and alias unit tests.
- Create `internal/modelcatalog/cache.go`: versioned cache records and TTL/stale orchestration.
- Create `internal/modelcatalog/cache_test.go`: cache safety, concurrency, TTL, and stale tests.
- Create `internal/modelcatalog/secure_unix.go`: no-follow reads and advisory refresh lock on Unix.
- Create `internal/modelcatalog/secure_other.go`: explicit unsupported-platform safety fallback.
- Create `internal/llm/openaip/catalog.go`: OpenAI-compatible and OpenRouter catalogue HTTP client.
- Create `internal/llm/openaip/catalog_test.go`: authenticated list and bounds tests.
- Create `internal/llm/anthropicp/catalog.go`: Anthropic paginated catalogue client.
- Create `internal/llm/anthropicp/catalog_test.go`: headers, pagination, and bounds tests.
- Create `cmd/waffle/provider_catalog.go`: preset normalization, catalogue factory, formatting, and browsing helpers.
- Create `cmd/waffle/provider_catalog_test.go`: preset, factory, browsing, and JSON tests.
- Modify `internal/providerconfig/manager.go`: expose a safe in-memory connection snapshot and transactionally add one favourite alias.
- Modify `internal/providerconfig/manager_test.go`: transaction, rollback, collision, and credential tests.
- Modify `cmd/waffle/provider_cmd.go`: route catalogue/model-add commands, integrate interactive discovery, and invalidate caches after removal.
- Modify `cmd/waffle/provider_cmd_test.go`: command compatibility, discovery ordering, warning-success, and redaction tests.
- Modify `cmd/waffle/provider_runtime.go`: reuse exported provider default URLs.
- Modify `README.md`, `docs/deploy.md`, and `config.example.toml`: operator workflow and examples.

### Infra

- Modify `scripts/waffle-admin.sh`: classify `provider model add` as mutating.
- Modify `tests/waffle_admin_lifecycle_test.rb`: prove reconciliation after model addition.

---

### Task 1: Provider-neutral catalogue domain

**Files:**
- Create: `internal/modelcatalog/catalog.go`
- Create: `internal/modelcatalog/catalog_test.go`

**Interfaces:**
- Consumes: `config.ProviderConnectionNameMax` and `config.ValidModelAlias`.
- Produces: `Model`, `Source`, `Connection`, `Normalize`, `Search`, and `AliasFor` for all later tasks.

- [ ] **Step 1: Write failing domain tests**

Create table-driven tests named `TestNormalizeModels`, `TestSearchModels`, and `TestAliasFor` with these exact cases:

```go
func TestAliasFor(t *testing.T) {
	tests := []struct {
		id   string
		want string
		err  bool
	}{
		{id: "anthropic/claude-sonnet-4.6", want: "anthropic-claude-sonnet-4-6"},
		{id: " GPT-5.4 ", want: "gpt-5-4"},
		{id: "///", err: true},
		{id: strings.Repeat("a", 80), want: strings.Repeat("a", config.ProviderConnectionNameMax)},
	}
	for _, tc := range tests {
		got, err := AliasFor(tc.id)
		if tc.err {
			if err == nil { t.Fatalf("AliasFor(%q) succeeded", tc.id) }
			continue
		}
		if err != nil || got != tc.want { t.Fatalf("AliasFor(%q) = %q, %v; want %q", tc.id, got, err, tc.want) }
	}
}
```

`TestNormalizeModels` must assert empty IDs, negative context windows, strings over 4096 bytes, Unicode control characters including newline and ESC, and more than 10,000 entries fail; duplicate IDs deduplicate; IDs and capability lists sort deterministically. `TestSearchModels` must assert case-insensitive substring matching over ID and display name and exact-ID matching. `TestSafeTextEscapesControls` must assert newline and ESC are rendered as visible `\\n` and `\\u001b` sequences.

- [ ] **Step 2: Run tests and confirm the missing API failure**

Run: `go test ./internal/modelcatalog -run 'Test(NormalizeModels|SearchModels|AliasFor)' -count=1`

Expected: FAIL because package `internal/modelcatalog` and its exported API do not exist.

- [ ] **Step 3: Implement the domain contract**

Create these definitions and keep normalization provider-neutral:

```go
const (
	MaxModels     = 10_000
	MaxFieldBytes = 4 * 1024
)

type Model struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	ContextWindow int64    `json:"context_window,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

type Source interface {
	ListModels(context.Context) ([]Model, error)
}

type Connection struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	ScopeID string `json:"scope_id"`
}

func Normalize(models []Model) ([]Model, error)
func Search(models []Model, query string) []Model
func AliasFor(upstream string) (string, error)
func SafeText(value string) string
```

`Normalize` validates before sorting, rejects Unicode control characters in every provider-supplied string, copies input so callers cannot mutate cached slices, deduplicates exact IDs, sorts capabilities, and returns a deterministic ID-sorted slice. `AliasFor` lowercases, replaces every non-`[a-z0-9]` run with `-`, trims hyphens, truncates to 64 bytes, trims a trailing hyphen after truncation, and validates through `config.ValidModelAlias`. `SafeText` escapes control runes with Go-style visible escapes and is used only for bounded provider error text, never to make an invalid descriptor acceptable.

- [ ] **Step 4: Run domain tests**

Run: `go test ./internal/modelcatalog -run 'Test(NormalizeModels|SearchModels|AliasFor)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the domain**

```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "feat: add provider model catalogue domain"
```

### Task 2: OpenAI and OpenRouter catalogue adapter

**Files:**
- Create: `internal/llm/openaip/catalog.go`
- Create: `internal/llm/openaip/catalog_test.go`
- Modify: `internal/llm/openaip/openai.go`
- Modify: `cmd/waffle/provider_runtime.go`

**Interfaces:**
- Consumes: `modelcatalog.Model`, `modelcatalog.Normalize`.
- Produces: `openaip.DefaultBaseURL`, `openaip.NewCatalog(apiKey, baseURL string, userFiltered bool) modelcatalog.Source`.

- [ ] **Step 1: Write failing authenticated HTTP tests**

Add tests named:

```go
func TestCatalogListsAuthenticatedOpenAIModels(t *testing.T)
func TestCatalogAllowsAuthFreeOpenAICompatibleEndpoint(t *testing.T)
func TestCatalogUsesOpenRouterUserModelsAndFallsBackOn404(t *testing.T)
func TestCatalogRejectsOversizedMalformedAndNonSuccessResponses(t *testing.T)
func TestCatalogHonorsCancellation(t *testing.T)
```

The first server must assert `GET /v1/models`, `Authorization: Bearer test-key`, then return:

```json
{"data":[{"id":"gpt-5.4","owned_by":"openai"},{"id":"openai/gpt-5","name":"GPT-5","context_length":400000,"supported_parameters":["tools","temperature"],"architecture":{"output_modalities":["text"]}}]}
```

Assert the normalized descriptors contain `tool-calling` and `text-output`. The OpenRouter test must observe `/v1/models/user` first and `/v1/models` only after a 404. The error table must cover 500 with a bounded body, malformed JSON, and a response larger than 16 MiB. The auth-free test must assert the Authorization header is absent.

- [ ] **Step 2: Run the adapter tests and confirm failure**

Run: `go test ./internal/llm/openaip -run Catalog -count=1`

Expected: FAIL because `NewCatalog` and `DefaultBaseURL` do not exist.

- [ ] **Step 3: Implement the bounded OpenAI-compatible client**

Add:

```go
const DefaultBaseURL = "https://api.openai.com/v1"

type Catalog struct {
	APIKey      string
	BaseURL     string
	Client      *http.Client
	UserFiltered bool
}

func NewCatalog(apiKey, baseURL string, userFiltered bool) *Catalog {
	if baseURL == "" { baseURL = DefaultBaseURL }
	return &Catalog{
		APIKey: apiKey,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client: &http.Client{Timeout: 10 * time.Second},
		UserFiltered: userFiltered,
	}
}

func (c *Catalog) ListModels(ctx context.Context) ([]modelcatalog.Model, error)
```

Use a 16 MiB `io.LimitedReader` plus one byte to detect overflow, cap non-success text at 4096 bytes, escape control characters in error text, and parse `data[].id`, `name`, `owned_by`, `context_length`, `supported_parameters`, and `architecture.output_modalities`. Reject a descriptor field containing the active non-empty key. Map `tools` or `tool_choice` to `tool-calling`, and a `text` output modality to `text-output`. Call `modelcatalog.Normalize` before returning. Only a 404 from `/models/user` may fall back to `/models`. Configure the catalogue HTTP client with `CheckRedirect` returning `http.ErrUseLastResponse` so credentials never cross an origin through redirects.

Replace the duplicated OpenAI default URL in `provider_runtime.go` with `openaip.DefaultBaseURL`; preserve runtime behavior exactly.

- [ ] **Step 4: Run focused and runtime tests**

Run: `go test ./internal/llm/openaip ./cmd/waffle -run 'Catalog|ModelRuntimeResolver' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the adapter**

```bash
git add internal/llm/openaip/catalog.go internal/llm/openaip/catalog_test.go internal/llm/openaip/openai.go cmd/waffle/provider_runtime.go
git commit -m "feat: discover OpenAI-compatible model catalogues"
```

### Task 3: Anthropic catalogue adapter

**Files:**
- Create: `internal/llm/anthropicp/catalog.go`
- Create: `internal/llm/anthropicp/catalog_test.go`
- Modify: `internal/llm/anthropicp/anthropic.go`

**Interfaces:**
- Consumes: `modelcatalog.Model`, `modelcatalog.Normalize`.
- Produces: `anthropicp.DefaultBaseURL`, `anthropicp.NewCatalog(apiKey, baseURL string) modelcatalog.Source`.

- [ ] **Step 1: Write failing pagination and authentication tests**

Add:

```go
func TestCatalogListsAllAnthropicPages(t *testing.T)
func TestCatalogStopsAtAnthropicPageLimit(t *testing.T)
func TestCatalogRejectsOversizedMalformedAndNonSuccessResponses(t *testing.T)
func TestCatalogHonorsCancellation(t *testing.T)
```

The pagination server must assert `x-api-key: test-key`, an `anthropic-version` header, `limit=1000`, and a second request carrying `after_id=model-1`. Return one page with `has_more:true,last_id:model-1`, then a terminal page. Assert `id`, `display_name`, and `max_input_tokens` map exactly. The page-limit server must continue returning `has_more:true` and prove request 101 is never made.

- [ ] **Step 2: Run tests and confirm failure**

Run: `go test ./internal/llm/anthropicp -run Catalog -count=1`

Expected: FAIL because the catalogue client does not exist.

- [ ] **Step 3: Implement paginated listing with the pinned SDK**

Add:

```go
const DefaultBaseURL = "https://api.anthropic.com"

type Catalog struct {
	client anthropic.Client
}

func NewCatalog(apiKey, baseURL string) *Catalog
func (c *Catalog) ListModels(ctx context.Context) ([]modelcatalog.Model, error)
```

Build the SDK client with `option.WithAPIKey`, `option.WithBaseURL`, and an HTTP client whose timeout is 10 seconds and whose `CheckRedirect` returns `http.ErrUseLastResponse`. Call `client.Models.List` with `Limit: param.NewOpt(int64(1000))`, manually call `GetNextPage`, count pages, and reject page 101. Bound each HTTP response at 16 MiB through a wrapping `RoundTripper`, reject any descriptor field containing the active key, accumulate no more than 10,000 descriptors, then call `modelcatalog.Normalize`. Export and reuse `DefaultBaseURL` without changing completion behavior.

- [ ] **Step 4: Run adapter and existing provider tests**

Run: `go test ./internal/llm/anthropicp -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the adapter**

```bash
git add internal/llm/anthropicp/catalog.go internal/llm/anthropicp/catalog_test.go internal/llm/anthropicp/anthropic.go
git commit -m "feat: discover Anthropic model catalogues"
```

### Task 4: Secure 24-hour catalogue cache

**Files:**
- Create: `internal/modelcatalog/cache.go`
- Create: `internal/modelcatalog/cache_test.go`
- Create: `internal/modelcatalog/secure_unix.go`
- Create: `internal/modelcatalog/secure_other.go`

**Interfaces:**
- Consumes: `modelcatalog.Connection`, `Model`, `Normalize`.
- Produces: `Store`, `Result`, `Load`, `Save`, `GetOrRefresh`, and `Invalidate` for the CLI.

- [ ] **Step 1: Write failing cache behavior tests**

Create tests named:

```go
func TestStoreFreshHitMakesNoRefreshRequest(t *testing.T)
func TestStoreExpiredAndForcedRefreshMakeOneRequest(t *testing.T)
func TestStoreConcurrentRefreshMakesOneRequest(t *testing.T)
func TestStoreRefreshFailureReturnsStaleRecord(t *testing.T)
func TestStoreRejectsMismatchedCorruptAndSymlinkRecords(t *testing.T)
func TestStoreWritesPrivateModesAtomically(t *testing.T)
func TestStoreFailedRefreshPreservesGoodBytes(t *testing.T)
func TestStoreInvalidateRemovesOnlyNamedConnection(t *testing.T)
```

Use an injected clock fixed at `2026-07-19T12:00:00Z`. Run two `Store` instances against the same directory in the concurrency test and block the fetch callback so both contend; assert its atomic counter is exactly one under `-race`. Assert stale results include age, `Stale:true`, and a sanitized warning but return no error.

- [ ] **Step 2: Run cache tests and confirm failure**

Run: `go test -race ./internal/modelcatalog -run Store -count=1`

Expected: FAIL because the cache API does not exist.

- [ ] **Step 3: Implement records, TTL, locking, and durable writes**

Use these public shapes:

```go
const (
	SchemaVersion = 1
	DefaultTTL = 24 * time.Hour
)

type Record struct {
	SchemaVersion int       `json:"schema_version"`
	Connection    Connection `json:"connection"`
	FetchedAt     time.Time  `json:"fetched_at"`
	Models        []Model    `json:"models"`
}

type Result struct {
	Record
	Age     time.Duration `json:"-"`
	Stale   bool          `json:"stale"`
	Warning string        `json:"warning,omitempty"`
}

type Store struct {
	Root string
	TTL  time.Duration
	Now  func() time.Time
}

func NewStore(home string) *Store
func (s *Store) Load(Connection) (Result, error)
func (s *Store) Save(Connection, []Model, time.Time) error
func (s *Store) GetOrRefresh(context.Context, Connection, bool, func(context.Context) ([]Model, error)) (Result, error)
func (s *Store) Invalidate(connection string) error
```

Normalize scheme/host case and trailing slashes before comparison. Reject `Save` when `Connection.ScopeID` is empty. Treat a missing record, schema mismatch, or connection/type/base-URL/scope mismatch as an internal cache miss: it is never returned as stale and `GetOrRefresh` proceeds to the authenticated refresh callback. Use `<home>/cache/model-catalogs/<connection>.json`; validate the connection slug before path construction. Implement no-follow regular-file reads on Unix with `unix.Open(...O_NOFOLLOW...)` and `unix.Fstat`. Open the per-connection refresh lock itself with `O_NOFOLLOW|O_CREAT`, reject non-regular files, enforce mode `0600`, then use nonblocking `unix.Flock` retry that checks the caller context every 25 ms; re-read after acquiring it. The non-Unix file must fail closed for no-follow reads and use a process mutex for tests/build compatibility.

Write through `os.CreateTemp` in the cache directory, `Chmod(0600)`, encode, `Sync`, close, `Rename`, and sync the directory. Never serialize warnings. `GetOrRefresh` returns a fresh record without fetching, refreshes one expired/forced record, and returns a valid stale record with warning if refresh fails. A missing valid cache plus refresh failure returns the refresh error.

- [ ] **Step 4: Run race and cache tests**

Run: `go test -race ./internal/modelcatalog -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the cache**

```bash
git add internal/modelcatalog/cache.go internal/modelcatalog/cache_test.go internal/modelcatalog/secure_unix.go internal/modelcatalog/secure_other.go
git commit -m "feat: cache provider model catalogues"
```

### Task 5: Transactional favourite addition and catalogue snapshots

**Files:**
- Modify: `internal/providerconfig/manager.go`
- Modify: `internal/providerconfig/manager_test.go`

**Interfaces:**
- Consumes: existing `Manager.acquire`, `recoverLocked`, `capture`, `stageConfig`, `stageSecrets`, `setModel`, `connectionKey`, `Probe`, and `commit`.
- Produces: `AddModelRequest`, `CatalogSnapshot`, `Manager.AddModel`, and `Manager.CatalogSnapshot` for CLI commands.

- [ ] **Step 1: Write failing manager tests**

Add:

```go
func TestManagerAddModelCommitsExactAliasAndRoles(t *testing.T)
func TestManagerAddModelProbeFailureRollsBack(t *testing.T)
func TestManagerAddModelLifecycleFailureRollsBack(t *testing.T)
func TestManagerAddModelRejectsUnknownConnectionInvalidAliasAndCollision(t *testing.T)
func TestManagerCatalogSnapshotReturnsConnectionAndDecryptedKey(t *testing.T)
func TestManagerCatalogSnapshotSupportsAuthFreeAndRejectsUnknownConnection(t *testing.T)
func TestManagerEnrollmentScopesDifferAcrossRemoveAndReAdd(t *testing.T)
func TestManagerCatalogSnapshotBackfillsLegacyScopeUnderLock(t *testing.T)
```

The success table must cover no role, default only, utility only, and both roles. Its probe must assert alias, connection name, upstream model, and `providerTestKey`. Every failure case snapshots config, secrets, ready-generation, and service state before the call and proves exact restoration. Assert no returned error contains `providerTestKey`.

- [ ] **Step 2: Run focused tests and confirm failure**

Run: `go test ./internal/providerconfig -run 'TestManager(AddModel|CatalogSnapshot)' -count=1`

Expected: FAIL because the request types and methods do not exist.

- [ ] **Step 3: Add the manager API**

Add:

```go
type AddModelRequest struct {
	ConnectionName string
	Alias          string
	UpstreamModel  string
	Default        bool
	Utility        bool
}

type CatalogSnapshot struct {
	Connection config.ProviderConnection
	APIKey     string
	ScopeID    string
}

// Manager.Random defaults to crypto/rand.Reader and is injectable in tests.
// Add this field to Manager:
Random io.Reader

func (m *Manager) CatalogSnapshot(ctx context.Context, name string) (snapshot CatalogSnapshot, err error)
func (m *Manager) AddModel(ctx context.Context, req AddModelRequest) (err error)
```

`CatalogSnapshot` acquires the provider lock, recovers, loads configuration, rejects an unknown connection, resolves its encrypted key through `connectionKey`, and loads the opaque secret `provider/<name>/catalog-scope`. If an existing connection lacks that scope, read 32 bytes from `Manager.Random` (defaulting to `crypto/rand.Reader`), hex-encode them, and persist the scope through the encrypted store while still under the provider lock. Release the lock before returning. Never serialize or log `APIKey` or `ScopeID` outside the owner-only cache record. Treat scope-generation failure as a catalogue-access failure without modifying configuration or service state.

Update `Manager.Add` to generate and stage a new random catalogue scope with the credential, and update `Manager.Remove` to delete both the API-key and catalogue-scope secrets. `AddModel` validates all input before staging. Under the existing mutation lease it captures state, rejects an unknown connection or existing alias, writes only `[models.<alias>]` and requested agent role keys, loads the staged config, and compares all unrelated provider/model/agent fields with the original. Stage secrets through a no-op mutation so rollback retains the same two-resource transaction. Resolve the staged target, decrypt the existing connection key, probe, and call `commit` with that key for redaction.

Use this exact model mutation in the staged document:

```go
target := config.ModelTarget{Provider: req.ConnectionName, Model: req.UpstreamModel}
setModel(doc, req.Alias, target)
if req.Default {
	doc.setValue("agent", "default_model", strconv.Quote(req.Alias))
}
if req.Utility {
	doc.setValue("agent", "utility_model", strconv.Quote(req.Alias))
}
```

- [ ] **Step 4: Run focused and race tests**

Run: `go test -race ./internal/providerconfig -run 'TestManager(AddModel|CatalogSnapshot)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the manager change**

```bash
git add internal/providerconfig/manager.go internal/providerconfig/manager_test.go
git commit -m "feat: add transactional provider model favourites"
```

### Task 6: Provider presets and catalogue service

**Files:**
- Create: `cmd/waffle/provider_catalog.go`
- Create: `cmd/waffle/provider_catalog_test.go`
- Modify: `cmd/waffle/provider_cmd.go`

**Interfaces:**
- Consumes: provider adapters, `modelcatalog.Store`, `providerconfig.CatalogSnapshot`.
- Produces: preset normalization, effective connection classification, an injectable catalogue service, and formatting helpers for Tasks 7 and 8.

- [ ] **Step 1: Write failing preset, URL, classification, and service tests**

Add tests named:

```go
func TestProviderPresetDefaultsAndOverrides(t *testing.T)
func TestProviderPresetRejectsUnsafeAndMissingURLs(t *testing.T)
func TestCatalogConnectionRecognizesOpenRouterAfterReload(t *testing.T)
func TestCatalogConnectionTreatsCustomOverrideAsGenericOpenAI(t *testing.T)
func TestProviderCatalogServiceNewEnrollmentNeverReadsCache(t *testing.T)
func TestProviderCatalogServiceUsesFreshExpiredAndForcedCache(t *testing.T)
func TestProviderCatalogServiceRedactsRefreshErrors(t *testing.T)
```

The preset table must assert:

```go
var presetCases = []struct {
	input, runtimeType, storedBase, effectiveBase string
}{
	{"openai", "openai", "", openaip.DefaultBaseURL},
	{"anthropic", "anthropic", "", anthropicp.DefaultBaseURL},
	{"openrouter", "openai", "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1"},
}
```

Also assert `openai-compatible` requires an absolute HTTP(S) base URL; every URL rejects userinfo, query, and fragment. Reload a connection containing `https://eu.openrouter.ai/api/v1`, delete its cache, and assert the factory requests `/models/user`. Override OpenRouter to `https://gateway.example/v1` and assert it uses `/models`.

- [ ] **Step 2: Run tests and confirm failure**

Run: `go test ./cmd/waffle -run 'Test(ProviderPreset|CatalogConnection|ProviderCatalogService)' -count=1`

Expected: FAIL because `provider_catalog.go` does not exist.

- [ ] **Step 3: Implement presets and the injectable service**

Create these command-local contracts:

```go
type providerPreset struct {
	Name           string
	RuntimeType    string
	StoredBaseURL  string
}

type providerCatalogue interface {
	Discover(context.Context, modelcatalog.Connection, string) (modelcatalog.Result, error)
	Models(context.Context, modelcatalog.Connection, string, bool) (modelcatalog.Result, error)
	Save(modelcatalog.Connection, []modelcatalog.Model, time.Time) error
	Invalidate(string) error
}

var openProviderCatalogue = defaultProviderCatalogue

func resolveProviderPreset(kind, override string) (providerPreset, error)
func effectiveCatalogConnection(name string, connection config.ProviderConnection, scopeID string) (modelcatalog.Connection, bool, error)
func newCatalogueSource(modelcatalog.Connection, string, bool) (modelcatalog.Source, error)
```

`Discover` calls the source directly and returns in-memory descriptors with `FetchedAt=now`; it must not call `Store.Load` or `Store.Save`, and its pre-enrollment connection may have an empty scope. `Models` requires the non-empty scope from `CatalogSnapshot` and delegates to `Store.GetOrRefresh`. The default service resolves `config.Home()` and uses `<home>/cache/model-catalogs`.

OpenRouter detection parses the normalized URL and returns true only for host `openrouter.ai` or suffix `.openrouter.ai`; ports do not affect the hostname comparison. Generic OpenAI connections use `/models`. Map `openrouter` and `openai-compatible` to persisted runtime type `openai`.

- [ ] **Step 4: Run command-level service tests**

Run: `go test ./cmd/waffle -run 'Test(ProviderPreset|CatalogConnection|ProviderCatalogService)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit presets and service**

```bash
git add cmd/waffle/provider_catalog.go cmd/waffle/provider_catalog_test.go cmd/waffle/provider_cmd.go
git commit -m "feat: add provider catalogue presets"
```

### Task 7: Catalogue browsing and explicit favourite commands

**Files:**
- Modify: `cmd/waffle/provider_catalog.go`
- Modify: `cmd/waffle/provider_catalog_test.go`
- Modify: `cmd/waffle/provider_cmd.go`
- Modify: `cmd/waffle/provider_cmd_test.go`

**Interfaces:**
- Consumes: `providerManager.CatalogSnapshot`, `providerManager.AddModel`, `providerCatalogue.Models`, `providerCatalogue.Invalidate`, `modelcatalog.AliasFor`, and `modelcatalog.Search`.
- Produces: `provider models`, `provider model add`, and cache invalidation after provider removal.

- [ ] **Step 1: Extend the fake manager and write failing command tests**

Extend `providerManager` and `fakeProviderManager` with:

```go
AddModel(context.Context, providerconfig.AddModelRequest) error
CatalogSnapshot(context.Context, string) (providerconfig.CatalogSnapshot, error)
```

Add:

```go
func TestProviderModelsCommandUsesCacheSearchRefreshAndStableJSON(t *testing.T)
func TestProviderModelsCommandNeverLeaksCredential(t *testing.T)
func TestProviderModelAddCommandGeneratesAndForwardsAliasAndRoles(t *testing.T)
func TestProviderModelAddCommandAcceptsExactUncachedID(t *testing.T)
func TestProviderRemoveInvalidatesCatalogueAfterCommit(t *testing.T)
func TestProviderRemoveCacheFailureWarnsAndReturnsSuccess(t *testing.T)
func TestProviderRemoveFailureDoesNotInvalidateCatalogue(t *testing.T)
```

Stable JSON must have this shape:

```go
type catalogueOutput struct {
	Connection   string               `json:"connection"`
	FetchedAt    time.Time            `json:"fetched_at"`
	AgeSeconds   int64                `json:"age_seconds"`
	Stale        bool                 `json:"stale"`
	Warning      string               `json:"warning,omitempty"`
	Models       []modelcatalog.Model `json:"models"`
}
```

The remove/re-add regression must simulate a prior account cache, make invalidation fail after successful removal, enroll the same name with a different key, assert the new `ScopeID` differs, and prove the old cache is inapplicable even though its bytes remain.

- [ ] **Step 2: Run command tests and confirm failure**

Run: `go test ./cmd/waffle -run 'TestProvider(ModelsCommand|ModelAddCommand|Remove)' -count=1`

Expected: FAIL because the commands and interface methods are absent.

- [ ] **Step 3: Implement command routing and output**

Add usage and routing for:

```text
waffle provider models <connection> [--search QUERY] [--refresh] [--json]
waffle provider model add <connection> <upstream-id> [--alias ALIAS] [--default] [--utility]
```

Implement a small argument parser that accepts `provider models` flags both before and after the connection, rejects duplicates/unknown arguments, and never interprets model IDs as flags. Obtain the connection/key through `CatalogSnapshot`, build its effective catalogue connection, call `Models`, apply `modelcatalog.Search`, and render either human rows or `catalogueOutput`.

For model addition, generate an alias with `AliasFor` only when `--alias` is absent, construct:

```go
providerconfig.AddModelRequest{
	ConnectionName: connection,
	Alias: alias,
	UpstreamModel: upstream,
	Default: makeDefault,
	Utility: makeUtility,
}
```

Then call `Manager.AddModel`. Do not require cache membership. Preserve the existing activate/remove model commands.

After `Manager.Remove` succeeds, call `catalogue.Invalidate(name)`. If invalidation fails, print a sanitized warning and return success. Do not invalidate on manager failure.

- [ ] **Step 4: Run focused command tests**

Run: `go test ./cmd/waffle -run 'TestProvider(ModelsCommand|ModelAddCommand|Remove)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit browsing and favourite commands**

```bash
git add cmd/waffle/provider_catalog.go cmd/waffle/provider_catalog_test.go cmd/waffle/provider_cmd.go cmd/waffle/provider_cmd_test.go
git commit -m "feat: browse and favourite provider models"
```

### Task 8: Interactive discovery-led provider enrollment

**Files:**
- Modify: `cmd/waffle/provider_catalog.go`
- Modify: `cmd/waffle/provider_catalog_test.go`
- Modify: `cmd/waffle/provider_cmd.go`
- Modify: `cmd/waffle/provider_cmd_test.go`

**Interfaces:**
- Consumes: provider presets, `providerCatalogue.Discover`, `providerCatalogue.Save`, catalogue search/alias helpers, and existing `providerconfig.AddRequest` transaction.
- Produces: the approved guided `sudo waffle provider add` experience.

- [ ] **Step 1: Write failing interaction and ordering tests**

Add:

```go
func TestProviderCommandBareAddDiscoversAndSelectsDefaultUtilityAndFavourites(t *testing.T)
func TestProviderCommandSmallCatalogueDisplaysDirectly(t *testing.T)
func TestProviderCommandLargeCatalogueSearchesAndPaginatesTwenty(t *testing.T)
func TestProviderCommandAliasCollisionPromptsExplicitAlias(t *testing.T)
func TestProviderCommandDiscoveryFailureOffersManualEntryWithoutCache(t *testing.T)
func TestProviderCommandExplicitModelsBypassDiscovery(t *testing.T)
func TestProviderCommandNonInteractiveMissingModelFailsBeforeSecret(t *testing.T)
func TestProviderCommandPartialFlagAutomationDoesNotEnterGuidedDiscovery(t *testing.T)
func TestProviderCommandPreflightStillPrecedesSecretAndDiscovery(t *testing.T)
func TestProviderCommandFailedAddDoesNotSaveCatalogue(t *testing.T)
func TestProviderCommandCacheSaveFailureWarnsAfterCommittedSuccess(t *testing.T)
```

The primary interaction must select an OpenRouter preset without asking for a URL, search `claude sonnet`, choose one default, search and choose utility, add one more favourite, and assert the exact three `ModelTarget` entries plus default/utility aliases. Use 51 models to enter search mode and assert output pages never exceed 20 rows. The post-commit save-failure test must prove `fake.addRequest` was accepted, a post-commit `CatalogSnapshot` supplied the new scope, stdout reports provider success, stderr contains a sanitized cache warning, and the command returns nil.

- [ ] **Step 2: Run interaction tests and confirm failure**

Run: `go test ./cmd/waffle -run 'TestProviderCommand(BareAdd|SmallCatalogue|LargeCatalogue|AliasCollision|Discovery|ExplicitModels|NonInteractive|Preflight|FailedAdd|CacheSave)' -count=1`

Expected: FAIL on the old manual-first prompt flow.

- [ ] **Step 3: Implement non-prefetching prompts and selection**

Add helpers:

```go
func promptLineNoReadAhead(io.Reader, io.Writer, string, string) (string, error)
func selectCatalogueModel(io.Reader, io.Writer, string, []modelcatalog.Model, bool) (modelcatalog.Model, bool, error)
func selectFavouriteModels(io.Reader, io.Writer, []modelcatalog.Model, map[string]struct{}) (map[string]config.ModelTarget, string, string, error)
func renderCataloguePage(io.Writer, []modelcatalog.Model, int) (int, error)
```

`promptLineNoReadAhead` reads one byte at a time through newline with a 64 KiB bound so it cannot buffer secret input ahead of the hidden terminal reader. Results accept numbers, exact IDs, or case-insensitive search. Show all catalogues of at most 50 entries; otherwise require a search. Render 20 results per page and accept next, previous, and a replacement search.

Reorder bare add to: preset, default connection name, custom URL only when needed, manager open, preflight, existing alias listing, hidden key, one direct authenticated discovery, favourite selection, `Manager.Add`, post-commit `CatalogSnapshot`, then cache save using the committed scope. New enrollment must never call cache load or stale fallback. On discovery failure offer manual comma-separated `ALIAS=UPSTREAM` input. Explicit flag-based models bypass all discovery. `--api-key-stdin`, `--api-key-file`, or any partial flag-based automation without models errors before key input and never silently enters guided discovery. A post-commit snapshot or save failure emits a warning and returns success.

When generated aliases collide with existing or newly selected aliases, ask for and validate an explicit alias. Default and utility selections automatically enter the favourites map; `-` utility leaves utility empty so runtime falls back to default. If default, utility, and additional selection would otherwise leave the map empty, require one unassigned favourite before calling `Manager.Add`.

- [ ] **Step 4: Run command race tests**

Run: `go test -race ./cmd/waffle -run 'TestProviderCommand' -count=1`

Expected: PASS, including all pre-existing provider command tests.

- [ ] **Step 5: Commit guided enrollment**

```bash
git add cmd/waffle/provider_catalog.go cmd/waffle/provider_catalog_test.go cmd/waffle/provider_cmd.go cmd/waffle/provider_cmd_test.go
git commit -m "feat: guide provider enrollment with model discovery"
```

### Task 9: Infra lifecycle classification for model addition

**Files:**
- Modify in Infra: `scripts/waffle-admin.sh`
- Modify in Infra: `tests/waffle_admin_lifecycle_test.rb`

**Interfaces:**
- Consumes: Waffle command `provider model add <connection> <upstream-id> --default`.
- Produces: lifecycle reconciliation after the new mutating command; no deployment-input changes.

- [ ] **Step 1: Create a clean Infra worktree for execution**

Run from the Infra repository:

```bash
git worktree add -b codex/model-catalog-lifecycle .worktrees/model-catalog-lifecycle origin/main
```

Expected: a clean worktree at `infra/.worktrees/model-catalog-lifecycle`. Read its `AGENTS.md` before editing.

- [ ] **Step 2: Write the failing lifecycle regression**

Extend the fixture Waffle script with an exact `provider model add openai gpt-test --alias gpt --default` case that records one mutation and moves its fake lifecycle to Ready. Add:

```ruby
def test_model_addition_reconciles_ready_host
  with_host do |root, env|
    install_admin_fixture(root)
    File.write(File.join(root, "lifecycle-state"), "installed\n")
    File.write(File.join(root, "active"), "0\n")

    result = run_admin(root, env, "provider", "model", "add", "openai", "gpt-test",
                       "--alias", "gpt", "--default")

    assert result.last.success?, result_output(result)
    assert File.exist?(File.join(root, "enabled"))
    assert_includes command_lines(root),
                    "tailscale serve --bg --yes --https=443 http://127.0.0.1:8420"
    assert_equal ["provider model add openai gpt-test --alias gpt --default"],
                 File.readlines(File.join(root, "mutation.log"), chomp: true)
  end
end
```

- [ ] **Step 3: Run the Infra regression and confirm failure**

Run: `ruby tests/waffle_admin_lifecycle_test.rb`

Expected: FAIL because `provider model add` is not classified as mutating and lifecycle reconciliation does not run.

- [ ] **Step 4: Classify model addition as mutating**

Change only the nested provider-model case:

```bash
model)
  case "${original[2]:-}" in
    activate|add|remove) mutating=true ;;
  esac
  ;;
```

Do not add provider/model data to deployment inputs and do not weaken key-file requirements.

- [ ] **Step 5: Run all Waffle-focused Infra tests**

Run:

```bash
ruby tests/waffle_admin_lifecycle_test.rb
ruby tests/waffle_router_free_deployment_test.rb
ruby tests/deploy_waffle_workflow_test.rb
ruby tests/operate_waffle_workflow_test.rb
ruby tests/reconcile_waffle_workflow_test.rb
ruby tests/waffle_infrastructure_contract_test.rb
```

Expected: all runs and assertions pass.

- [ ] **Step 6: Commit the Infra change**

```bash
git add scripts/waffle-admin.sh tests/waffle_admin_lifecycle_test.rb
git commit -m "fix: reconcile Waffle model additions"
```

### Task 10: Documentation, compatibility, and delivery verification

**Files:**
- Modify: `cmd/waffle/provider_cmd.go`
- Modify: `README.md`
- Modify: `docs/deploy.md`
- Modify: `config.example.toml`
- Verify only: `../matt-riley-ci/.github/workflows/request-infra-deploy.yml`

**Interfaces:**
- Consumes: every implemented CLI command and cache behavior.
- Produces: an operator-ready workflow and evidence-backed delivery decision.

- [ ] **Step 1: Write failing documentation acceptance assertions**

Add `TestProviderDocumentationAcceptance` in `cmd/waffle/provider_catalog_test.go`. Read the three documentation files and assert they contain all of:

```go
required := []string{
	"openai-compatible",
	"openrouter",
	"provider models",
	"--refresh",
	"provider model add",
	"24 hours",
	"ALIAS=UPSTREAM",
}
```

Also assert `providerUsage` contains both new command forms and still states that API keys are never accepted as command-line values.

- [ ] **Step 2: Run the acceptance test and confirm failure**

Run: `go test ./cmd/waffle -run TestProviderDocumentationAcceptance -count=1`

Expected: FAIL listing missing catalogue documentation.

- [ ] **Step 3: Update operator documentation and help**

Update the guided deployment path to show:

```text
sudo waffle provider add
sudo waffle provider models openrouter --search claude
sudo waffle provider models openrouter --refresh
sudo waffle provider model add openrouter anthropic/claude-sonnet-4.6 --default
```

Explain presets, custom URL behavior, authenticated discovery, favourites, 24-hour owner-only cache, stale warnings, manual fallback, explicit automation, and disposable cache data. In `config.example.toml`, retain the provider-free default and add comments pointing operators to catalogue commands; do not add a configured provider or secret.

- [ ] **Step 4: Run focused compatibility tests**

Run:

```bash
go test ./cmd/waffle ./internal/providerconfig ./internal/config ./internal/modelcatalog ./internal/llm/... -count=1
```

Expected: PASS.

- [ ] **Step 5: Run the complete Waffle gate**

Run in order:

```bash
mise run fmt
mise run test
mise run vet
mise run lint
mise run build
```

Expected: every command exits zero; `mise run test` includes race tests and zero-network evals.

- [ ] **Step 6: Verify repository boundaries**

Inspect `matt-riley-ci/.github/workflows/request-infra-deploy.yml` and confirm its inputs remain source/artifact provenance plus GitHub App authentication only. Run `git diff --check` and `git status --short` in Waffle, Infra, and `matt-riley-ci`. Expected: only planned Waffle and Infra changes; no shared-CI diff, secrets, generated databases, cache records, or identity files.

- [ ] **Step 7: Commit documentation**

```bash
git add cmd/waffle/provider_cmd.go cmd/waffle/provider_catalog_test.go README.md docs/deploy.md config.example.toml
git commit -m "docs: explain provider model catalogues"
```

- [ ] **Step 8: Request final implementation review**

Use `superpowers:requesting-code-review` on the complete Waffle and Infra diffs. Resolve every correctness, security, transaction, compatibility, and UX finding; rerun the affected focused tests and then the full gates before publishing.

- [ ] **Step 9: Publish and verify deployment without claiming a live provider check**

After review approval and green local gates, push the focused commits through the repository's established main-branch workflow, monitor Waffle CI and Infra's Waffle deployment workflow, then verify on the host:

```bash
sudo waffle --version
sudo waffle provider list --json
sudo waffle provider models missing
sudo waffle provider help
```

Expected: the deployed version contains the catalogue commands; provider listing remains credential-free; a missing connection fails clearly without creating cache/config state; help shows the three presets and custom-compatible option. Do not claim authenticated model discovery works live until an operator supplies a real provider credential and authorizes that provider call.
