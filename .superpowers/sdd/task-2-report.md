# Task 2 report: foundational correctness and security (#8-#32)

## Status

Complete. Audited all 91 acceptance rows belonging to issues #8-#29 and #32. Issues #30 and #31 have no rows in the source matrix. The matrix now contains 90 `pass` dispositions and one justified `not-applicable` disposition (#29.5).

## Criterion dispositions

- #8.1-#8.3: pass — summaries are system text and Anthropic translation is covered.
- #9.1-#9.2: pass — accepted gateway work uses a detached drain context and persists on cancellation.
- #10.1-#10.3: pass — serve stops and awaits the scheduler before deferred shared-resource cleanup; scheduler shutdown drains cron work.
- #11.1-#11.5: pass — only definitive keyring not-found permits creation; transient errors and existing identities refuse without overwrite.
- #12.1-#12.3: pass — OpenAI streaming requests usage and consumes the empty-choice usage chunk.
- #13.1-#13.3: pass — text truncation cannot discard complete tool calls or rewrite tool-use completion.
- #14.1-#14.3: pass — first-heartbeat startup grace is distinct from dead-runner detection.
- #15.1-#15.3: pass — probe failures/contention fail fast rather than count as healthy.
- #16.1-#16.2: pass — only `ErrWorkspaceNotFound` enters workspace creation; transient lookup errors have no creation side effects.
- #17.1-#17.4: pass — scheduler reconciliation tracks entry IDs and observes additions/removals after start.
- #18.1-#18.2: pass — chat session state changes only after turn history loads.
- #19.1-#19.7: pass — workspace-close parsing rejects ambiguity/unknown flags before manager construction and preserves valid forms.
- #20.1-#20.6: pass — delivery parsing validates position, multiplicity, value shape, target, persisted prompt, and usage.
- #21.1-#21.2: pass — upgrade validates option-shaped refs before git and covers accepted refs.
- #22.1-#22.2: pass — `write_file` creates owner-only files (`0600`).
- #23.1-#23.8: pass — fetch performs dial-time private-address blocking, redirect rechecks, allowlisting, IPv4/IPv6 coverage, and refusal without body leakage; README documents the escape hatch.
- #24.1-#24.8: pass — migrations are set-based, contiguous/unique, transactional, idempotent, and exercise out-of-order application.
- #25.1-#25.6: pass — FTS delete/update triggers and rebuild preserve external-content integrity and search behavior.
- #26.1-#26.2: pass — unique races use typed SQLite result codes with a constrained fallback and resume the winner.
- #27.1: pass — memory note dates use UTC.
- #28.1-#28.2: pass — slash command routing uses exact command tokens and near-misses remain chat text.
- #29.1-#29.4, #29.6-#29.7: pass — opt-in stress/crash and Docker bind-mount harnesses, doctor probes, runbook, support outcome, and failure-mode guidance exist in-tree. Docker execution is gated on a daemon.
- #29.5: not-applicable — this task explicitly forbids editing GitHub issues; repository findings are recorded in `docs/sandbox-queue.md` instead.
- #32.1-#32.7: pass — bound-repo credential scope is deny-by-default, canonicalized, host-restricted, audited, non-leaking, and tested with fakes.

Exact owning symbols, test names, manual/gated evidence, and verification disposition are recorded on every individual row in `docs/acceptance-audit.md`.

## Files changed

- `docs/acceptance-audit.md` — updated all 91 cohort rows.
- `.superpowers/sdd/task-2-report.md` — this report.

No production or test files changed: inspection of the assertions showed the current implementation already covers the cohort. Therefore no behavioral gap required a new TDD cycle.

## Commands and results

1. Mapping and assertion inspection:
   - `rg -n 'func Test' cmd internal`
   - targeted `sed`/`rg` reads across the implementation and matching tests.
2. Full required cohort:
   - `go test ./internal/agent ./internal/broker ./internal/config ./internal/gateway ./internal/llm/... ./internal/sandbox ./internal/schedule ./internal/secret ./internal/session ./internal/store ./internal/tool ./internal/workspace ./cmd/waffle -count=1`
   - Result: PASS; all listed packages passed, `internal/llm` correctly reported no test files.
3. Required host stress suite:
   - `go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1`
   - Result: PASS (`ok .../internal/sandbox 2.411s`).
4. Docker availability gate:
   - `docker info --format '{{json .}}'`
   - Result: gated/unavailable (`zsh: command not found: docker`). The in-tree Docker-tagged test is designed to skip with an explicit reason when Docker is unavailable.

## TDD evidence

No new behavioral gap was found and no production behavior was changed, so RED/GREEN does not apply to this evidence-only audit update. Existing regression assertions were read before being accepted as evidence; test names alone were not treated as proof.

## Self-review

- Scope check: only cohort rows #8-#32 were edited; rows #33 onward remain unchanged.
- Status check: no cohort row remains `partial` or `fail`.
- Security check: negative evidence includes transient keyring failure, workspace lookup failure, ref option injection, private/link-local and redirect SSRF refusal, unbound/wrong-repo credential refusal, and non-GitHub host refusal.
- Criteria were not weakened and GitHub issues were not edited.
- The only N/A is the expressly unauthorized external issue-write requirement (#29.5), with the repository evidence location stated.

## Concerns

- Docker is absent on this machine, so the Docker Desktop/VirtioFS bind-mount command could not execute locally. This is an approved gated condition under the task brief; host queue stress passed and the opt-in Docker test/runbook remain available for a Docker-equipped macOS host.
- #29.5 cannot be performed without violating the explicit instruction not to edit GitHub issues.
