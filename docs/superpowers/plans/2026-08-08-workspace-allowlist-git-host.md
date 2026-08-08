# Workspace Allowlist Git Host Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a workspace's bound Git host reachable under `allowlist` egress without changing `full` or existing `none` behavior.

**Architecture:** Keep the behavior in the existing command-level workspace composition root. The broker continues to own egress targets; `newWorkspaceManager` continues to install the per-workspace Git-host callback, with only its mode guard widened to include `allowlist`.

**Tech Stack:** Go 1.25.12, standard `testing` package, existing broker/config/store/workspace packages.

## Global Constraints

- Preserve deny-by-default egress and leave `full` unrestricted.
- Preserve the existing `none` and empty/default behavior.
- Do not add dependencies, migrations, or broker API surface.
- Keep the change scoped to #239.
- Use table-driven Go tests beside the existing workspace broker tests.

---

### Task 1: Cover egress-mode callback wiring

**Files:**
- Modify: `cmd/waffle/ws_broker_test.go`

**Interfaces:**
- Consumes: `newWorkspaceManager(config.Config, *store.Store, *broker.Broker)`.
- Produces: `TestWorkspaceManagerWiresGitHostForBrokeredEgress`.

- [ ] **Step 1: Write the failing test**

Add a table-driven test that creates a broker-backed manager for `allowlist`, `none`, empty, and `full`, then asserts the callback matrix:

```go
func TestWorkspaceManagerWiresGitHostForBrokeredEgress(t *testing.T) {
    cases := []struct {
        name  string
        egress string
        want  bool
    }{
        {name: "default", egress: "", want: true},
        {name: "none", egress: "none", want: true},
        {name: "allowlist", egress: "allowlist", want: true},
        {name: "full", egress: "full", want: false},
    }

    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
            if err != nil {
                t.Fatal(err)
            }
            defer func() { _ = st.Close() }()

            cfg := config.Config{}
            cfg.Workspace.Egress = tc.egress
            mgr := newWorkspaceManager(cfg, st, broker.New(st, nil))

            if got := mgr.AllowGitHost != nil; got != tc.want {
                t.Fatalf("AllowGitHost present = %v, want %v", got, tc.want)
            }
        })
    }
}
```

- [ ] **Step 2: Run the focused test to verify it fails**

Run: `go test ./cmd/waffle -run '^TestWorkspaceManagerWiresGitHostForBrokeredEgress$' -count=1`

Expected: FAIL only for the `allowlist` case because the current mode guard omits `allowlist`.

- [ ] **Step 3: Implement the minimal wiring change**

Modify the existing guard in `cmd/waffle/ws_cmd.go` from:

```go
if cfg.Workspace.Egress == "none" || cfg.Workspace.Egress == "" {
```

to:

```go
switch cfg.Workspace.Egress {
case "allowlist", "none", "":
```

and retain the existing callback body unchanged.

- [ ] **Step 4: Run focused tests**

Run: `gofmt -w cmd/waffle/ws_cmd.go cmd/waffle/ws_broker_test.go && go test ./cmd/waffle -run '^TestWorkspaceManagerWiresGitHostForBrokeredEgress$' -count=1`

Expected: PASS.

- [ ] **Step 5: Run repository verification**

Run:
```bash
mise run fmt
mise run vet
mise run lint
mise run test
```

Expected: each command exits 0 with no test failures.

- [ ] **Step 6: Commit**

```bash
git add cmd/waffle/ws_cmd.go cmd/waffle/ws_broker_test.go
git commit -m "fix(#239): keep workspace git host reachable under allowlist"
```
