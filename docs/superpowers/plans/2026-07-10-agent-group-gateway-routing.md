# Agent-Group Gateway Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route each persisted gateway conversation through the agent policy named by its channel group, failing closed for unavailable groups.

**Architecture:** `channel_groups.agent_group` remains the durable session binding. `gateway.Gateway` receives an agent registry and resolves it after loading the channel group. `serve` builds the main tier plus every configured tier and owns all cleanup functions for the process lifetime.

**Tech Stack:** Go, SQLite, Go standard testing.

## Global Constraints

- Preserve `main` as the default for new channel groups.
- Never fall back from an unavailable persisted group to `main`.
- Do not add channel-to-group configuration; #34 owns assignment policy.
- Keep existing callers using `Gateway.Agent` working as the `main` fallback.

---

### Task 1: Add gateway registry regression coverage

**Files:**

- Modify: `internal/gateway/gateway_test.go`
- Modify: `internal/gateway/gateway.go`

**Interfaces:**

- Produces: `Gateway.Agents map[string]*agent.Agent` and `agentFor(group string) (*agent.Agent, error)`.

- [ ] Write `TestGatewayUsesPersistedAgentGroup`: reopen the store after binding a channel group to `restricted`, then assert its message uses a `restricted` provider.
- [ ] Run `go test ./internal/gateway -run TestGatewayUsesPersistedAgentGroup -count=1`; expect failure because the gateway always invokes `Gateway.Agent`.
- [ ] Add `Agents` and `agentFor`; select the resolved agent inside `converse`, and return `gateway: no agent configured for group <name>` for absent entries.
- [ ] Write and run `TestGatewayRejectsUnavailablePersistedAgentGroup`, proving no main-agent fallback occurs.
- [ ] Run `go test ./internal/gateway -run 'TestGateway(UsesPersistedAgentGroup|RejectsUnavailablePersistedAgentGroup)' -count=1`; expect pass.

### Task 2: Build and retain all configured gateway tiers

**Files:**

- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/agent_group_test.go`

**Interfaces:**

- Consumes: `Gateway.Agents` from Task 1.
- Produces: a server gateway with `main` and every `[agent.group.<name>]` built once.

- [ ] Extract the server's group-agent construction into a testable helper returning an agent map and one cleanup callback. Write `TestConfiguredGatewayGroupBuildsRegistryEntry`: a configured group appears in that registry and its toolbox omits denied `bash`, while main retains it.
- [ ] Run `go test ./cmd/waffle -run TestConfiguredGatewayGroupBuildsRegistryEntry -count=1`; expect failure because serve constructs only main and cron locally.
- [ ] Build `main`, cron, and every non-main configured group through the helper; retain cleanup callbacks and invoke them in reverse order. Pass the map to the gateway and retain cron as the scheduler agent.
- [ ] Run `go test ./cmd/waffle -run 'Test(AgentGroup|ConfiguredGatewayGroup)' -count=1`; expect pass.

### Task 3: Verify and close the issue

**Files:**

- Modify: `README.md` only if its trust-tiering section needs the persisted-resolution behavior documented.

- [ ] Run `gofmt -w internal/gateway/gateway.go internal/gateway/gateway_test.go cmd/waffle/serve_cmd.go cmd/waffle/agent_group_test.go`.
- [ ] Run `go test ./... && go vet ./...`; expect both commands to exit 0.
- [ ] Reconcile #33 against its acceptance checklist, then close it with a concise verification comment.
