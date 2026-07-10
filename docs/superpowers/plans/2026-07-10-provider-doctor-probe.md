# Provider Doctor Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `waffle doctor` block upgrades when a configured provider cannot authenticate or respond.

**Architecture:** Add a focused provider-probe helper in `internal/selfdev` that resolves the configured key, constructs the existing adapter, and runs a one-token completion under a five-second context. `Doctor` records its outcome through its existing check accumulator.

**Tech Stack:** Go 1.25, existing `internal/config`, `internal/secret`, and provider adapters; `net/http/httptest`.

## Global Constraints

- Probe exactly one configured provider with no tools, no persisted conversation, and `MaxTokens: 1`.
- Use a five-second child context and perform no retries.
- Missing provider key is a passing skipped check; invalid configuration, secret resolution, authentication, or deadline failures are red.

---

### Task 1: Provider probe helper and tests

**Files:**

- Modify: `internal/selfdev/selfdev.go`
- Modify: `internal/selfdev/selfdev_test.go`

**Interfaces:**

- Produces: `providerCheck(context.Context, config.Provider) (string, error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestProviderCheck(t *testing.T) {
	// Test an authenticated OpenAI-compatible httptest SSE response, absent
	// key skip, HTTP 401, and a deadline-bound blocked server.
}
```

- [ ] **Step 2: Run it red**

Run: `go test -race ./internal/selfdev -run '^TestProviderCheck$'`

Expected: FAIL because `providerCheck` is undefined.

- [ ] **Step 3: Add the minimal implementation**

```go
func providerCheck(ctx context.Context, p config.Provider) (string, error) {
	key, err := secret.ResolveRef(p.APIKey, providerEnvName(p.Name))
	if err != nil { return "", err }
	if key == "" { return "no API key configured (skipped)", nil }
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	provider, err := doctorProvider(p, key)
	if err != nil { return "", err }
	_, err = provider.Complete(ctx, llm.Request{Model: p.Model, MaxTokens: 1, Messages: []llm.Message{llm.UserText("health check")}}, nil)
	return "authenticated completion", err
}
```

`doctorProvider` selects the existing Anthropic or OpenAI-compatible adapter, and defaults the OpenAI base URL to `https://api.openai.com/v1`.

- [ ] **Step 4: Run it green**

Run: `go test -race ./internal/selfdev -run '^TestProviderCheck$'`

Expected: PASS.

### Task 2: Integrate the probe with doctor

**Files:**

- Modify: `internal/selfdev/selfdev.go`
- Modify: `internal/selfdev/selfdev_test.go`

**Interfaces:**

- Consumes: `providerCheck(context.Context, config.Provider) (string, error)`.
- Produces: a `provider reachable` check and false aggregate health for any probe failure.

- [ ] **Step 1: Write the failing aggregation test**

```go
func TestDoctorReportsProviderFailure(t *testing.T) {
	// A config targeting a 401 httptest endpoint returns ok=false and includes
	// a failed provider reachable check.
}
```

- [ ] **Step 2: Run it red**

Run: `go test -race ./internal/selfdev -run '^TestDoctorReportsProviderFailure$'`

Expected: FAIL because Doctor does not append a provider check.

- [ ] **Step 3: Add the doctor check**

```go
info, err := providerCheck(ctx, cfg.Provider)
add("provider reachable", err, info)
```

Place it after secret-store handling and before the sandbox-runner check.

- [ ] **Step 4: Validate and publish**

Run: `go test -race ./internal/selfdev && go test -race ./... && go vet ./... && gofmt -l .`

Then stage only the selfdev files, commit `feat: probe provider in doctor`, push `main`, and close issue #37.
