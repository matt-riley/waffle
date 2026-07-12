# All-Issues Acceptance Audit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Audit every acceptance criterion in all 64 closed GitHub issues, repair every unsupported criterion, and publish current criterion-level evidence.

**Architecture:** A generated issue snapshot feeds a human-reviewed Markdown evidence matrix. Audit work is split into chronological issue cohorts, but every row uses the same status and evidence contract. Remediation follows test-driven development in the owning Go package; final verification updates the matrix only from commands run against the completed tree.

**Tech Stack:** Go 1.25.12, GitHub CLI, jq, ripgrep, Go test/race/vet, golangci-lint, Waffle deterministic eval harness, Markdown.

## Global Constraints

- Audit all 64 closed issues returned by `gh issue list --state all`, including unchecked criteria in closed issues.
- Treat issue checkbox and closure state as claims, not evidence.
- Use `pass`, `partial`, `fail`, or justified `not-applicable` exactly as defined in the approved design.
- Infrastructure-dependent criteria may pass only through deterministic gated tests plus documented manual evidence.
- Do not weaken, merge away, or silently reinterpret an issue criterion.
- Do not edit GitHub issue state or text.

---

### Task 1: Freeze the issue criteria and establish the evidence matrix

**Files:**
- Create: `docs/acceptance-audit.md`
- Create: `docs/acceptance-audit/issues.json`

**Interfaces:**
- Consumes: GitHub issues for `matt-riley/waffle`.
- Produces: one immutable source snapshot and one matrix row per checkbox or explicit completion condition, keyed as `#<issue>.<criterion-index>`.

- [ ] **Step 1: Export the complete issue snapshot**

Run:

```bash
mkdir -p docs/acceptance-audit
gh issue list --repo matt-riley/waffle --state all --limit 200 \
  --json number,title,state,body,labels,closedAt,updatedAt,url \
  > docs/acceptance-audit/issues.json
jq -e 'length == 64 and all(.state == "CLOSED")' docs/acceptance-audit/issues.json
```

Expected: `jq` exits 0.

- [ ] **Step 2: Create the matrix header and evidence contract**

Create `docs/acceptance-audit.md` with this exact opening structure:

```markdown
# Acceptance-Criteria Audit

Source snapshot: `docs/acceptance-audit/issues.json`

| ID | Criterion | Status | Implementation/artifact | Automated evidence | Manual/gated evidence | Current verification |
|---|---|---|---|---|---|---|
```

Append one row for every issue checkbox and explicit prose completion condition. Preserve criterion wording verbatim; escape Markdown table pipes without changing meaning. Initially set status to `partial` and evidence cells to `unreviewed`.

- [ ] **Step 3: Prove snapshot and matrix completeness**

Run a small read-only comparison that counts issue acceptance checkboxes in the JSON and corresponding matrix IDs:

```bash
jq -r '.[] | .number as $n | (.body // "") | split("\n")[] | select(test("^[[:space:]]*- \\[.[xX ]?\\]")) | $n' docs/acceptance-audit/issues.json | sort -n | uniq -c
rg '^\| #[0-9]+\.[0-9]+ ' docs/acceptance-audit.md | sed -E 's/^\| #([0-9]+)\..*/\1/' | sort -n | uniq -c
```

Expected: counts match after manually accounting in the matrix for explicit prose conditions and alternative terminal-state criteria.

- [ ] **Step 4: Commit the source-of-truth artifacts**

```bash
git add docs/acceptance-audit.md docs/acceptance-audit/issues.json
git commit -m "docs: inventory issue acceptance criteria"
```

### Task 2: Audit foundational correctness and security issues #8–#32

**Files:**
- Modify: `docs/acceptance-audit.md`
- Modify when a gap is found: owning files under `cmd/waffle/`, `internal/agent/`, `internal/broker/`, `internal/config/`, `internal/gateway/`, `internal/llm/`, `internal/sandbox/`, `internal/schedule/`, `internal/secret/`, `internal/session/`, `internal/store/`, `internal/tool/`, or `internal/workspace/`
- Test when a gap is found: matching `*_test.go` in the owning package

**Interfaces:**
- Consumes: matrix rows for #8–#32 and the current implementation.
- Produces: criterion-level evidence and regression-tested fixes for context handling, shutdown, secrets, streaming, sandbox queues, scheduling, command parsing, filesystem permissions, SSRF, migrations, FTS, time handling, workspace races, and repository scoping.

- [ ] **Step 1: Map each criterion to implementation and tests**

For every #8–#32 row, record exact symbols/files and exact test names. Use:

```bash
rg -n '#(8|9|10|11|12|13|14|15|16|17|18|19|20|21|22|23|24|25|26|27|28|29|30|31|32)\b' README.md docs cmd internal
rg -n 'func Test' cmd internal
```

Do not pass a row based only on a nearby test name; inspect its assertions.

- [ ] **Step 2: Add a failing regression test for each behavioral gap**

Place each test in the owning package and name it after the precise missing behavior, following this form:

```go
func TestAcceptanceIssueNNMissingBehavior(t *testing.T) {
    // Arrange the issue's boundary condition.
    // Invoke the public or package boundary used in production.
    // Assert the complete acceptance result and negative side effects.
}
```

Run only the new test and confirm it fails for the intended reason:

```bash
go test ./internal/<owner> -run '^TestAcceptanceIssueNNMissingBehavior$' -count=1
```

- [ ] **Step 3: Implement the minimal correction and rerun focused tests**

Modify only the owning production boundary. Run:

```bash
go test ./internal/<owner> -run 'TestAcceptanceIssueNNMissingBehavior|<ExistingRelatedTest>' -count=1
```

Expected: PASS.

- [ ] **Step 4: Verify the full cohort**

```bash
go test ./internal/agent ./internal/broker ./internal/config ./internal/gateway ./internal/llm/... ./internal/sandbox ./internal/schedule ./internal/secret ./internal/session ./internal/store ./internal/tool ./internal/workspace ./cmd/waffle -count=1
go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1
```

Expected: PASS. Docker-gated tests may skip only under the approved evidence rule.

- [ ] **Step 5: Update and commit the cohort evidence**

Set every cohort row to `pass` or justified `not-applicable`, including the command result. No `partial` or `fail` may be committed as complete.

```bash
git add docs/acceptance-audit.md cmd internal README.md docs
git commit -m "audit: verify acceptance criteria for issues 8 through 32"
```

### Task 3: Audit trust, operations, and lifecycle issues #33–#58

**Files:**
- Modify: `docs/acceptance-audit.md`
- Modify when required: owning production and test files under `cmd/waffle/` and `internal/`
- Modify when gated/manual evidence is required: `README.md`, `docs/deploy.md`, `docs/plan.md`, `docs/sandbox-queue.md`, or `SECURITY.md`

**Interfaces:**
- Consumes: matrix rows for #33–#58.
- Produces: evidence and fixes for trust tiers, group posture, egress, lifecycle, provider checks, search, extension decisions, credentials, portability, limits, memory write safety, recovery, resource controls, data deletion, status, hooks, repo policy, intake, retry, and memory maintenance.

- [ ] **Step 1: Inspect every criterion and its negative path**

Use issue text and targeted search:

```bash
jq -r '.[] | select(.number >= 33 and .number <= 58) | "#\(.number) \(.title)\n\(.body)\n"' docs/acceptance-audit/issues.json
rg -n 'func Test' cmd/waffle internal/{backup,broker,channel,config,gateway,gitcred,hooks,intake,mcp,memory,observability,repopolicy,selfdev,session,store,tool,workspace}
```

For security boundaries, require tests that prove denied actions never reach the executor, network, credential mint, live prompt file, or destructive store operation.

- [ ] **Step 2: Use TDD for each discovered gap**

Write a focused failing test, run it alone, make the smallest production change, and rerun it. For documentation/manual-evidence gaps, add exact prerequisites, commands, expected output, and failure interpretation to the owning document.

- [ ] **Step 3: Verify the cohort**

```bash
go test ./cmd/waffle ./internal/backup ./internal/broker ./internal/channel/... ./internal/config ./internal/gateway ./internal/gitcred ./internal/hooks ./internal/intake ./internal/mcp ./internal/memory ./internal/observability ./internal/repopolicy ./internal/selfdev ./internal/session ./internal/store ./internal/tool ./internal/workspace -count=1
```

Expected: PASS.

- [ ] **Step 4: Validate platform-gated artifacts**

```bash
go test -tags=sandbox_docker ./internal/sandbox -run BindMount -count=1
go test ./internal/selfdev -run 'Sandbox|Provider' -count=1
```

Expected: deterministic tests pass; unavailable Docker may skip only when the matrix links the documented manual runbook.

- [ ] **Step 5: Update and commit the cohort evidence**

```bash
git add docs/acceptance-audit.md cmd internal README.md SECURITY.md docs
git commit -m "audit: verify acceptance criteria for issues 33 through 58"
```

### Task 4: Audit agent quality, memory, policy, and code-intelligence issues #59–#79

**Files:**
- Modify: `docs/acceptance-audit.md`
- Modify when required: `evals/*.toml`, `cmd/waffle/*`, and owning files/tests under `internal/agent/`, `internal/codeintel/`, `internal/config/`, `internal/eval/`, `internal/mcp/`, `internal/memory/`, `internal/policy/`, `internal/session/`, `internal/skill/`, `internal/spill/`, `internal/workset/`, and `internal/workspace/`

**Interfaces:**
- Consumes: matrix rows for #59–#79.
- Produces: evidence and fixes for gateway reflection, retrieval, summarization, self-dev gates, evals, queue identity, learning, action policy, working sets, subagent broadcast, spill recovery, maintenance, profiles, restricted MCP, structured handoffs, and code intelligence.

- [ ] **Step 1: Map criteria to behavior-level tests and evals**

```bash
jq -r '.[] | select(.number >= 59 and .number <= 79) | "#\(.number) \(.title)\n\(.body)\n"' docs/acceptance-audit/issues.json
rg -n 'func Test' internal/{agent,codeintel,config,eval,mcp,memory,policy,session,skill,spill,workset,workspace} cmd/waffle
for f in evals/*.toml; do printf '%s\n' "$f"; sed -n '1,220p' "$f"; done
```

Inspect assertions for downgrade reasons, observed/reported verification, stale code-intelligence data, environment isolation, policy ordering, concurrent snapshots, and deterministic eval failure behavior.

- [ ] **Step 2: Use TDD for every gap**

Add the narrowly failing unit, integration, or eval case first. For an eval gap, add a TOML scenario with a concrete scripted provider exchange and assertion; run:

```bash
go run ./cmd/waffle eval
```

Expected before correction: the new scenario fails for the intended missing behavior. Expected after correction: all evals pass.

- [ ] **Step 3: Verify the cohort including concurrency**

```bash
go test ./cmd/waffle ./internal/agent ./internal/codeintel ./internal/config ./internal/eval ./internal/mcp ./internal/memory ./internal/policy ./internal/session ./internal/skill ./internal/spill ./internal/workset ./internal/workspace -count=1
go test -race ./internal/agent ./internal/session ./internal/workset ./internal/workspace -count=1
go run ./cmd/waffle eval
```

Expected: PASS.

- [ ] **Step 4: Update and commit the cohort evidence**

```bash
git add docs/acceptance-audit.md cmd internal evals README.md docs
git commit -m "audit: verify acceptance criteria for issues 59 through 79"
```

### Task 5: Cross-check matrix integrity and eliminate unsupported passes

**Files:**
- Modify: `docs/acceptance-audit.md`

**Interfaces:**
- Consumes: completed cohort evidence.
- Produces: a matrix with no missing criteria, duplicate IDs, unsupported pass claims, or unexplained alternative outcomes.

- [ ] **Step 1: Reconcile every issue against the matrix**

Reread the JSON issue-by-issue and compare every checkbox and prose completion condition to its row. Confirm dependencies such as #25→#47 and #77→#79 remain separately evidenced.

- [ ] **Step 2: Reject incomplete statuses**

```bash
rg '^\| #[0-9]+\.[0-9]+ .*\| (partial|fail) \|' docs/acceptance-audit.md
rg '^\| #[0-9]+\.[0-9]+ .*\| pass \| (unreviewed|none|N/A)' docs/acceptance-audit.md
```

Expected: both commands return no matches.

- [ ] **Step 3: Validate every evidence reference**

For each referenced file, symbol, and test name, use `rg` to prove it exists. Replace line numbers with stable symbol/test references where practical so the audit survives unrelated edits.

- [ ] **Step 4: Commit matrix corrections**

```bash
git add docs/acceptance-audit.md
git commit -m "docs: reconcile acceptance audit evidence"
```

### Task 6: Run repository-wide verification and record final evidence

**Files:**
- Modify: `docs/acceptance-audit.md`

**Interfaces:**
- Consumes: fully remediated repository and reconciled matrix.
- Produces: current full-suite evidence and final per-issue totals.

- [ ] **Step 1: Check formatting and build**

```bash
mise run fmt
mise run build
```

Expected: PASS and `bin/waffle` produced.

- [ ] **Step 2: Run vet, tests, race detector, and deterministic evals**

```bash
mise run vet
mise run test
```

Expected: PASS.

- [ ] **Step 3: Run lint when available**

```bash
if command -v golangci-lint >/dev/null 2>&1; then mise run lint; else echo 'golangci-lint unavailable; deterministic CI/manual evidence required'; fi
```

Expected: lint passes, or the unavailable-tool condition is recorded honestly with the repository's CI/manual evidence.

- [ ] **Step 4: Run deterministic tagged suites**

```bash
go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1
go test -tags=sandbox_docker ./internal/sandbox -run BindMount -count=1
```

Expected: stress suite passes; Docker suite passes or skips under the documented gated-test rule.

- [ ] **Step 5: Record final command results and issue totals**

Add a final verification section to `docs/acceptance-audit.md` containing the exact command, date, result, and any gated skip. Add a per-issue summary table whose criterion totals reconcile to the matrix.

- [ ] **Step 6: Verify the worktree and commit the final audit**

```bash
git diff --check
git status --short
git add docs/acceptance-audit.md
git commit -m "docs: finalize all-issues acceptance audit"
```

Expected: no uncommitted audit changes remain, and every matrix row is `pass` or justified `not-applicable`.
