# Managed Multi-Provider Deployment Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` to execute this plan task-by-task. Follow `superpowers:test-driven-development` for every behavior change and `superpowers:verification-before-completion` before claiming completion.

**Goal:** Make Waffle deployable without operator-managed database, router, or provider credentials, then let an operator add and validate multiple named provider connections on the installed host.

**Architecture:** Waffle remains a single Go binary backed by embedded SQLite. Configuration gains named provider connections and deterministic model aliases. A transactional `waffle provider` command manages encrypted credentials and configuration after installation. Infrastructure installs a provider-empty system in `Installed` state, promotes it to `Ready` after the first validated default model, and no longer deploys Workweave Router or Postgres. Shared CI carries immutable artifact provenance only.

**Tech Stack:** Go 1.25.12, Cobra, TOML, encrypted age secret store, systemd, Bash, GitHub Actions, Terraform, Ruby/Python contract tests.

---

## Global constraints

- Work in isolated worktrees for `waffle`, `../infra`, and `../matt-riley-ci`; preserve the existing uncommitted Waffle identity changes until Task 4 deliberately incorporates them.
- Write a failing test before each behavior change. Observe the expected failure, make the smallest implementation pass, then refactor while green.
- Keep the legacy singular `[provider]` configuration readable while adding the new registry.
- Never accept provider API keys through command-line arguments, workflow inputs, or committed environment files. Permit a hidden terminal prompt, `--api-key-stdin`, or a mode-checked `--api-key-file`.
- Keep infrastructure provider-blind. It may install Waffle and inspect Waffle's lifecycle state, but it must not know provider types, model names, or API keys.
- Do not remove the old router runtime from a live host until direct-provider `Ready` has been proven.
- Publish dependencies in order: shared CI v2 contract, Waffle caller, then Infra deployment changes.

## Task 1: Release the generic immutable-artifact handoff in shared CI

**Repository:** `../matt-riley-ci`

**Files:**

- Create: `tests/contract_workflow_registration_test.py`
- Modify: `.github/workflows/contract-tests.yml`
- Modify: `.github/workflows/request-infra-deploy.yml`
- Modify: `tests/request_infra_deploy_contract_test.py`
- Modify: `README.md`

### Step 1: Add a failing workflow-registration test

Create a test that loads `.github/workflows/contract-tests.yml` and proves both contract suites are executed:

```python
def test_contract_workflow_runs_infra_deploy_contracts():
    workflow = Path(".github/workflows/contract-tests.yml").read_text()
    assert "tests/request_infra_deploy_contract_test.py" in workflow
    assert "tests/contract_workflow_registration_test.py" in workflow
```

Run:

```bash
python -m unittest tests/contract_workflow_registration_test.py
```

Expected: FAIL because the artifact-aware infra-deploy contract is not registered.

### Step 2: Register and strengthen the contract

- Run the registration test and `tests/request_infra_deploy_contract_test.py` from the contract-test workflow.
- Preserve the generic inputs `artifact_name`, `artifact_run_id`, and `artifact_digest`.
- Reject partial artifact provenance: all three values must be supplied together.
- Forward the exact values to the Infra repository dispatch without interpreting application-specific details.
- Update README examples to call `mattriley/matt-riley-ci/.github/workflows/request-infra-deploy.yml@v2`.

### Step 3: Verify and publish the v2 contract

Run:

```bash
python -m unittest discover -s tests -p '*test.py'
actionlint
git diff --check
```

Commit:

```bash
git add .github/workflows/contract-tests.yml .github/workflows/request-infra-deploy.yml README.md tests
git commit -m "fix: release immutable infra artifact handoff"
```

After review, push the branch and move the `v2` major tag only after the commit is green. Record the immutable commit SHA for Task 5.

## Task 2: Add named providers and deterministic model aliases to Waffle config

**Repository:** `waffle`

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.toml`

### Step 1: Write failing registry and compatibility tests

Add table-driven tests for:

- two named connections with different provider types;
- multiple model aliases resolving to the same connection;
- aliases resolving to different connections;
- unknown alias, unknown provider reference, invalid connection name, unsupported provider type, and missing upstream model;
- legacy `[provider]` normalization without changing existing behavior;
- default and utility model aliases.

Run:

```bash
go test ./internal/config -run 'Test(LoadProviderRegistry|ResolveModel|LegacyProviderCompatibility)'
```

Expected: FAIL because the registry types and resolver do not exist.

### Step 2: Implement the config model

Add these public shapes, adapting existing provider fields rather than duplicating semantics:

```go
type ProviderConnection struct {
	Type      string `toml:"type"`
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	MaxTokens int    `toml:"max_tokens"`
}

type ModelTarget struct {
	Provider  string `toml:"provider"`
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
}

type ResolvedModel struct {
	Alias          string
	ConnectionName string
	Connection     ProviderConnection
	UpstreamModel  string
	MaxTokens      int
}
```

Extend `Config` with `Providers map[string]ProviderConnection` and `Models map[string]ModelTarget`, and extend agent configuration with `DefaultModel` and `UtilityModel`. Implement `ResolveModel(alias)` with deterministic lookup and clear validation errors. Normalize the legacy singular `[provider]` into one internal connection only when the registry is absent.

### Step 3: Update the example and verify

Show one Anthropic connection and one OpenAI-compatible connection, with aliases mapped explicitly. Explain that an installed system may omit all providers.

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
go test ./internal/config
git diff --check
```

Commit:

```bash
git add internal/config config.example.toml
git commit -m "feat: add named provider configuration"
```

## Task 3: Resolve provider clients from model aliases at runtime

**Repository:** `waffle`

**Files:**

- Create: `cmd/waffle/provider_runtime.go`
- Create: `cmd/waffle/provider_runtime_test.go`
- Modify: `cmd/waffle/chat_cmd.go`
- Modify: `cmd/waffle/serve_cmd.go`
- Modify: `cmd/waffle/learn_cmd.go`
- Modify: relevant command and agent-profile tests beside those files

### Step 1: Write failing runtime-resolution tests

Use fake factories and providers to prove:

- an alias selects its named connection and upstream model;
- aliases on one connection reuse one client;
- aliases on different provider types create isolated clients;
- per-model max tokens override the connection default;
- secret resolution occurs at the connection boundary and redaction remains attached to that client;
- chat, serve, learn, default-model, utility-model, and profile overrides all resolve aliases rather than passing aliases upstream.

Run:

```bash
go test ./cmd/waffle -run 'Test(ModelRuntimeResolver|ChatModelAlias|ServeModelAlias|LearnModelAlias)'
```

Expected: FAIL because runtime code still constructs one global provider.

### Step 2: Implement the resolver

Use injectable factories and a per-connection cache:

```go
type providerFactory func(apiKey, baseURL string) llm.Provider
type secretResolver func(config.ProviderConnection) (string, func(string) string, error)

type modelRuntimeResolver struct {
	cfg       config.Config
	factories map[string]providerFactory
	secrets   secretResolver
	clients   map[string]llm.Provider
	redactors map[string]func(string) string
}
```

Register native `anthropic` and `openai` factories. Treat OpenAI, OpenRouter, Ollama, and custom compatible endpoints as named `openai` connections distinguished by `base_url`. Do not implement automatic failover, price routing, or implicit provider selection.

### Step 3: Verify and commit

Run:

```bash
gofmt -w cmd/waffle
go test ./cmd/waffle ./internal/agent ./internal/llm/...
git diff --check
```

Commit:

```bash
git add cmd/waffle internal/agent internal/llm
git commit -m "feat: route model aliases to provider connections"
```

## Task 4: Add transactional on-host provider management

**Repository:** `waffle`

**Files:**

- Create: `internal/providerconfig/manager.go`
- Create: `internal/providerconfig/manager_test.go`
- Create: `cmd/waffle/provider_cmd.go`
- Create: `cmd/waffle/provider_cmd_test.go`
- Modify: `cmd/waffle/main.go`
- Modify: `internal/secret/identity.go`
- Modify: `internal/secret/identity_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

### Step 1: Preserve and prove identity bootstrap behavior

Carry the pre-existing identity changes into the isolated worktree. Keep the invariant already covered by their tests: `secret init --print` works without a desktop keyring, while ordinary initialization stays fail-closed.

Run:

```bash
go test ./internal/secret -run TestIdentity
```

### Step 2: Write failing transaction tests

Define an injectable probe and request:

```go
type Probe func(context.Context, config.ResolvedModel, string) error

type AddRequest struct {
	ConnectionName string
	Connection     config.ProviderConnection
	Models         map[string]config.ModelTarget
	DefaultModel   string
	UtilityModel   string
	APIKey         string
}

type Status struct {
	State        string `json:"state"`
	DefaultModel string `json:"default_model,omitempty"`
}
```

Test:

- an exclusive `$WAFFLE_HOME/provider-config.lock` blocks concurrent mutation;
- probe failure leaves config and secret store byte-for-byte unchanged;
- successful add stages both resources, commits the encrypted secret before the config reference, and clears backups only after restart plus health succeeds;
- restart or health failure restores both previous resources;
- TOML comments and unrelated settings survive mutation;
- removal is rejected while any alias references the connection;
- first validated default moves `Installed` to `Ready`; provider-empty remains `Installed`;
- errors and JSON never include the API key.

Run:

```bash
go test ./internal/providerconfig
```

Expected: FAIL because the manager package does not exist.

### Step 3: Implement the manager

Use an AST-preserving TOML writer. Stage config and encrypted secret-store files in their destination directories, fsync them, probe the candidate model, then commit secret before config with retained backups. Inject restart and health callbacks so unit tests never require systemd. Roll back on every failure after the first commit.

### Step 4: Write failing CLI tests

Cover this interface:

```text
waffle provider add [--name NAME] [--type anthropic|openai] [--base-url URL]
                    [--model ALIAS=UPSTREAM]... [--default ALIAS] [--utility ALIAS]
                    [--api-key-stdin | --api-key-file PATH]
waffle provider list [--json]
waffle provider test <connection>
waffle provider remove <connection>
```

Prove that raw `--api-key` is absent, interactive input is hidden, stdin works, key files must be regular and mode `0600`, list output redacts credentials, and remove reports references precisely.

Run:

```bash
go test ./cmd/waffle -run TestProviderCommand
```

Expected: FAIL because the command is not registered.

### Step 5: Implement the command and lifecycle status

- Default to a hidden terminal prompt; accept exactly one of prompt, stdin, or file.
- Let the operator create several aliases in one add operation.
- Probe the candidate through the same runtime resolver used by normal requests.
- Start or restart the service only after a validated default alias exists.
- Return stable JSON from `provider list --json`, including `state: installed|ready` and the configured default alias.

### Step 6: Verify and commit

Run:

```bash
gofmt -w cmd/waffle internal/providerconfig internal/secret
go test -race ./internal/providerconfig ./internal/secret ./cmd/waffle
git diff --check
```

Commit only the intended identity changes with this task:

```bash
git add cmd/waffle internal/providerconfig internal/secret go.mod go.sum
git commit -m "feat: manage providers on installed hosts"
```

## Task 5: Wire Waffle CI and document the operator contract

**Repository:** `waffle`

**Files:**

- Modify: `.github/workflows/ci.yml`
- Modify: `internal/eval/ci_workflow_test.go`
- Modify: `README.md`
- Modify: `docs/deployment.md` if present, otherwise create it
- Modify: `config.example.toml`

### Step 1: Add a failing workflow contract test

Assert that the deploy-request job:

- needs the immutable Linux artifact build;
- calls the released shared CI v2 contract;
- forwards artifact name, run ID, and digest;
- forwards no provider key or provider-specific input.

Run:

```bash
go test ./internal/eval -run TestCIWorkflow
```

Expected: FAIL because the deploy request was previously removed.

### Step 2: Restore generic artifact handoff

Call the exact reviewed v2 shared workflow and forward only immutable artifact provenance. Keep deployment reconciliation in Infra.

### Step 3: Document the two-state flow

Document:

1. deploy produces `Installed` with Waffle, SQLite, age identity, admin CLI, and no provider;
2. `sudo waffle provider add` validates and commits a connection;
3. the first validated default model starts the service and produces `Ready`;
4. further named providers and aliases may be added independently;
5. no Router, Postgres, or Pub/Sub emulator is part of Waffle deployment.

Include prompt, stdin, and `0600` file examples without showing real secrets.

### Step 4: Run full Waffle verification

```bash
mise run fmt
mise run test
mise run vet
mise run lint
mise run build
git diff --check
```

Commit:

```bash
git add .github/workflows/ci.yml internal/eval README.md docs config.example.toml
git commit -m "docs: define provider-free installation flow"
```

## Task 6: Remove Router and make Infra rollout lifecycle-aware

**Repository:** `../infra`

**Files:**

- Delete: `deploy/waffle/router-compose.yml`
- Delete: `.github/workflows/rotate-waffle-secrets.yml`
- Delete: `scripts/waffle-rotate-secrets.sh`
- Modify: `catalog/apps/waffle.yaml`
- Modify: `deploy/waffle/config.toml`
- Modify: `deploy/waffle/waffle.service`
- Modify: `scripts/waffle-bootstrap.sh`
- Modify: `scripts/waffle-rollout.sh`
- Modify: all directly corresponding tests under `tests/`

### Step 1: Add failing negative and lifecycle tests

Tests must reject every remaining reference to:

- Workweave Router checkout, image, keys, health endpoint, or `router_ref`;
- Waffle Postgres database, password, user, container, or Pub/Sub emulator;
- provider API keys in Terraform, workflows, templates, or scripts.

Add rollout scenarios proving:

- provider-empty config installs the artifact and reports `Installed` without starting Waffle;
- a `Ready` host restarts and passes health after an update;
- a failed update restores the prior binary/config and service state;
- contradictory state, such as `Ready` with an unhealthy service, fails loudly;
- age identity is generated once after the binary exists.

Run the focused Ruby tests identified by the existing suite, for example:

```bash
ruby -Itest tests/waffle_deploy_workflow_test.rb
ruby -Itest tests/waffle_rollout_test.rb
ruby -Itest tests/waffle_infra_contract_test.rb
```

Expected: FAIL on current router-heavy deployment behavior.

### Step 2: Simplify the deploy assets

- Ship a provider-empty Waffle config using embedded SQLite.
- Keep Docker because Waffle's sandbox requires it, but remove all Router/Postgres containers.
- Install the versioned binary and an administrative `/usr/local/bin/waffle` entry point that supplies the managed `WAFFLE_HOME`.
- Initialize the age identity once through the installed binary.
- Query `waffle provider list --json` for lifecycle state.
- In `Installed`, replace the binary without starting the service.
- In `Ready`, restart, check health, and atomically roll back on failure.
- Leave Telegram disabled unless separately configured; do not make it an installation prerequisite.

### Step 3: Remove rotation machinery

Delete the Router credential-rotation workflow and script. Retain no compatibility shim because no Router credential remains.

### Step 4: Verify and commit

Run:

```bash
ruby -Itest -e 'Dir["tests/*waffle*test.rb"].sort.each { |f| require_relative f }'
bash -n scripts/waffle-bootstrap.sh scripts/waffle-rollout.sh
actionlint
git diff --check
```

Commit:

```bash
git add -A .github/workflows catalog/apps/waffle.yaml deploy/waffle scripts tests
git commit -m "feat: deploy Waffle without router infrastructure"
```

## Task 7: Add the zero-input Waffle operation workflow in Infra

**Repository:** `../infra`

**Files:**

- Create: `.github/workflows/operate-waffle.yml`
- Create: `tests/operate_waffle_workflow_test.rb`
- Modify: `.github/workflows/deploy-waffle.yml`
- Modify: `README.md`
- Modify: relevant Waffle deployment docs under `docs/`

### Step 1: Write a failing workflow test

Prove that `operate-waffle.yml`:

- exposes `workflow_dispatch` with no operator inputs;
- applies the Waffle Terraform target before host reconciliation;
- resolves the latest successful verified Waffle Linux artifact and its digest;
- invokes the same artifact verification and rollout path as routine repository dispatches;
- has concurrency and environment protection;
- accepts no database, router, provider, username, password, or secret input.

Run:

```bash
ruby -Itest tests/operate_waffle_workflow_test.rb
```

Expected: FAIL because the workflow does not exist.

### Step 2: Implement one-intent operation

Factor the existing deploy workflow into a reusable internal job if necessary. The manual workflow should mean only “make Waffle installed at the current approved version”: reconcile Terraform, resolve artifact provenance, verify digest, and run the lifecycle-aware rollout. Keep routine application artifact dispatch Terraform-free.

### Step 3: Replace obsolete documentation

Document the single manual action and the later on-host `waffle provider add` operation. Mark router-era design documents as superseded or remove them when they contain no remaining historical value.

### Step 4: Run full Infra verification

```bash
ruby -Itest -e 'Dir["tests/**/*_test.rb"].sort.each { |f| require_relative f }'
terraform fmt -check -recursive
terraform validate
actionlint
git diff --check
```

Commit:

```bash
git add .github/workflows README.md docs tests
git commit -m "feat: add one-step Waffle operation"
```

## Task 8: Review, publish, and prove the live migration

**Repositories:** all three

### Step 1: Run repository-wide gates

Waffle:

```bash
mise run fmt
mise run test
mise run vet
mise run lint
mise run build
git diff --check
```

Shared CI:

```bash
python -m unittest discover -s tests -p '*test.py'
actionlint
git diff --check
```

Infra:

```bash
ruby -Itest -e 'Dir["tests/**/*_test.rb"].sort.each { |f| require_relative f }'
terraform fmt -check -recursive
terraform validate
actionlint
git diff --check
```

### Step 2: Request broad code review

Review each task's commit range against the approved design. Resolve every critical or important finding and rerun the affected gates. Then perform a final cross-repository review for contract-version skew, secret exposure, rollback gaps, and stale Router references.

### Step 3: Publish in dependency order

1. Merge and release the shared CI v2 artifact contract.
2. Update Waffle to the released immutable contract reference, merge, and publish its artifact.
3. Merge Infra and run its protected operation workflow.

Do not move mutable tags or push protected branches unless the repository's normal reviewed release procedure authorizes it.

### Step 4: Execute live acceptance

On the target host:

1. Run the protected Waffle operation with zero inputs.
2. Prove `waffle provider list --json` reports `Installed` and the service is not required to be active.
3. Add one Anthropic connection and one OpenAI-compatible connection using hidden prompt, stdin, or secure files supplied out of band.
4. Exercise one model alias from each provider through Waffle.
5. Prove the first validated default yields `Ready` and healthy service state.
6. Deploy a newer artifact and prove the lifecycle-aware update.
7. Exercise an intentional health failure in the accepted non-production test path and prove rollback to the prior artifact.
8. Only after direct-provider `Ready` evidence exists, remove the old Router/Postgres runtime from the host and verify it does not return.

### Step 5: Produce the completion record

Record commit SHAs, workflow run URLs, artifact run ID and digest, host lifecycle output, both provider probes, update evidence, rollback evidence, and the exact Router resources removed. Report any unavailable external credential or protected-environment gate as a blocker rather than calling the deployment complete.
