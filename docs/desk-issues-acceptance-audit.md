# Waffle Desk issue acceptance reconciliation

This artifact reconciles the original 17-issue Desk scope for tracking issue
[#190](https://github.com/matt-riley/waffle/issues/190). It is an evidence
snapshot, not an inference from issue closure: each requirement below has its
own state, implementation reference, and verification or remaining gate.

The GitHub snapshot was collected with read-only `gh` commands on 2026-07-25.
The states below intentionally distinguish:

- **merged** — the requirement is represented by a merged implementation PR and
  the PR's direct verification record;
- **open/pending** — the requirement still needs final branch/PR verification;
- **externally owned** — the work is outside this run, even if GitHub now shows
  the issue or its PR as merged.

The current user scope explicitly makes #181, #182, #192, #193, and #194
externally owned. That ownership takes precedence over the GitHub state shown
in the evidence column. #188 and #195 remain in scope but are deliberately not
called complete.

## Requirement matrix

| Issue | Requirement | State | Direct implementation / commit / PR | Verification and remaining gate |
|---|---|---|---|---|
| [#173](https://github.com/matt-riley/waffle/issues/173) | AC1 — Each catalogue card can add its exact model without retyping connection or upstream model ID. | merged | PR [#219](https://github.com/matt-riley/waffle/pull/219), merge `86d3eb1`; branch `codex/issues-capabilities`, implementation commits `f871a0d`, `cbc10ae`, `373af7e`. | PR #219 records `node --test internal/dashboard/ui/capabilities_client_test.mjs` (16 passed), `mise run dashboard-check` (18 passed, 42 skipped), and full `mise run test`. |
| #173 | AC2 — Alias is editable, has a model-derived suggestion, and duplicate aliases produce a specific error. | merged | Same PR/commit set as AC1. | PR #219 verification plus the provider/model lane's duplicate-alias tests; no remaining gate recorded. |
| #173 | AC3 — Uses existing `POST /api/v1/desk/models` and restart-deferral/poll behavior. | merged | Same PR/commit set as AC1. | PR #219 records full race tests and dashboard checks; no mutation-contract change is reported. |
| #173 | AC4 — Already-enrolled catalogue models are labelled instead of offering a duplicate add. | merged | Same PR/commit set as AC1. | PR #219 summary explicitly records enrolled labels; client suite and browser gate passed. |
| #173 | AC5 — Client test refreshes a fake catalogue, activates add, and checks connection/model IDs in the POST body. | merged | Same PR/commit set as AC1. | PR #219 records the capabilities client suite (16 passed); direct test is `internal/dashboard/ui/capabilities_client_test.mjs`. |
| [#174](https://github.com/matt-riley/waffle/issues/174) | AC1 — Capabilities identifiers already present in the snapshot are selectable rather than required text input. | merged | PR [#219](https://github.com/matt-riley/waffle/pull/219), merge `86d3eb1`; commits `f871a0d`, `cbc10ae`, `373af7e`. | PR #219 records full race tests, `mise run dashboard-check` (18 passed, 42 skipped), and 16 client tests. |
| #174 | AC2 — Model cards can set default or utility role directly. | merged | Same PR/commit set as AC1. | PR #219 summary explicitly records direct default/utility actions; no remaining gate recorded. |
| #174 | AC3 — Pickers repopulate after successful mutation and after restart polling. | merged | Same PR/commit set as AC1. | PR #219 records dashboard/browser verification and the restart path was preserved; no remaining gate recorded. |
| #174 | AC4 — Empty prerequisites produce a named empty state such as “Enroll a provider first.” | merged | Same PR/commit set as AC1. | PR #219 summary records explicit empty states; client/browser gates passed. |
| #174 | AC5 — Client tests prove picker options come from the snapshot payload. | merged | Same PR/commit set as AC1. | `node --test internal/dashboard/ui/capabilities_client_test.mjs` — 16 passed, as recorded in PR #219. |
| [#175](https://github.com/matt-riley/waffle/issues/175) | AC1 — Provider type is selected from supported types and unsupported values cannot submit. | merged | PR [#219](https://github.com/matt-riley/waffle/pull/219), merge `86d3eb1`; commits `f871a0d`, `cbc10ae`, `373af7e`. | PR #219 records guided provider presets and full repository verification. |
| #175 | AC2 — Base URL is required exactly when the selected provider type requires it, with guidance before submit. | merged | Same PR/commit set as AC1. | PR #219 summary records conditional Base URL guidance; browser/client gates passed. |
| #175 | AC3 — Enrollment changes the Waffle-wide default only when explicitly requested, and the request body carries that choice. | merged | Same PR/commit set as AC1. | PR #219 summary records explicit role choices; full race suite and client tests passed. |
| #175 | AC4 — Desk can test a connection and distinguishes auth failure, unreachable endpoint, and success. | merged | Same PR/commit set as AC1. | PR #219 summary records protected prospective/persisted tests and safe outcome classification; full tests passed. |
| #175 | AC5 — Credential input clears on success and failure and is never echoed. | merged | Same PR/commit set as AC1. | PR #219 records the sensitive-intent retention fix and clean re-review; full race suite passed. |
| [#176](https://github.com/matt-riley/waffle/issues/176) | AC1 — Skill audit is a distinct pass/fail block with each flag listed and programmatically distinguishable. | merged | PR [#222](https://github.com/matt-riley/waffle/pull/222), merge `8c13096`; implementation/review commits `592aefb`, `d6779fd`, `79b5b10`, `b1ee3ce`, `607e9fc`. | PR #222 records 58 dashboard client tests, 11/11 zero-network evaluations, and final lifecycle re-review clean. |
| #176 | AC2 — Files are listed with path/size/digest and readable per-file previews, not one JSON string. | merged | Same PR/commit set as AC1. | PR #222 summary records readable review with files, digests, and previews; `mise run dashboard-check` 18 passed, 42 skipped. |
| #176 | AC3 — Remaining stage lifetime is displayed and expiry disables Install with a re-stage message. | merged | Same PR/commit set as AC1. | PR #222 summary records expiry gates; full `mise run test` and browser checks passed. |
| #176 | AC4 — Manifest values remain text-only and are never inserted as HTML. | merged | Same PR/commit set as AC1. | PR #222 summary records safe skill review; client tests and full race suite passed. |
| #176 | AC5 — Client test covers failed audit flags and does not present Install as the primary path. | merged | Same PR/commit set as AC1. | `node --test internal/dashboard/ui/capabilities_client_test.mjs` — 22 passed, recorded in PR #222. |
| [#177](https://github.com/matt-riley/waffle/issues/177) | AC1 — Snapshot exposes only configured skill import roots and Git hosts needed by the form. | merged | PR [#222](https://github.com/matt-riley/waffle/pull/222), merge `8c13096`; commits `592aefb` through `607e9fc`. | PR #222 records source allowlist labels and safe installer guidance; full race suite passed. |
| #177 | AC2 — Empty allowlists disable the stage form and name the required configuration keys. | merged | Same PR/commit set as AC1. | PR #222 records deny-by-default local/Git choices and dashboard checks (18 passed, 42 skipped). |
| #177 | AC3 — Configured allowlists display beside their relevant fields. | merged | Same PR/commit set as AC1. | PR #222 summary explicitly records source allowlist labels; client/browser gates passed. |
| #177 | AC4 — Local and Git sources are mutually exclusive; Git commit is required and validated as a full 40-hex SHA only on the Git path. | merged | Same PR/commit set as AC1. | PR #222 summary records exact commit validation and deny-by-default choices; full tests passed. |
| #177 | AC5 — Go snapshot coverage and a client test cover the disabled-by-default state. | merged | Same PR/commit set as AC1. | PR #222 records 58 client tests and full race suite; no remaining gate recorded. |
| [#178](https://github.com/matt-riley/waffle/issues/178) | AC1 — Active skills can be deactivated through Desk using the existing activation machinery. | merged | PR [#222](https://github.com/matt-riley/waffle/pull/222), merge `8c13096`; commits `592aefb`, `d6779fd`, `79b5b10`, `b1ee3ce`, `607e9fc`. | PR #222 records deactivate controls, focused lifecycle races, and final multi-pass review clean. |
| #178 | AC2 — Inactive skills can be uninstalled, with a specific refusal while attachments exist. | merged | Same PR/commit set as AC1. | PR #222 records attachment protections and client/browser gates passed. |
| #178 | AC3 — Deactivate and uninstall write `policy_audit` rows. | merged | Same PR/commit set as AC1. | PR #222 records full race tests and audit-preserving mutation behavior; no remaining gate recorded. |
| #178 | AC4 — Sessions with a missing skill resume with an actionable missing-capability state. | merged | Same PR/commit set as AC1. | PR #222 summary records lifecycle behavior; full `mise run test` passed. |
| #178 | AC5 — Go tests cover deactivate/uninstall/refusal and a client test covers controls. | merged | Same PR/commit set as AC1. | PR #222 records focused lifecycle/store/dashboard/cmd races and 22 capabilities client tests. |
| [#181](https://github.com/matt-riley/waffle/issues/181) | AC1 — Workspace cards show branch, dirty/file count, ahead/behind, and last commit without close flow. | externally owned (GitHub merged) | External PR [#220](https://github.com/matt-riley/waffle/pull/220), merge `6806578`; head `feat/181-182-desk-git-github`. | PR #220 records the projection and browser/client verification. This row is not owned or accepted by this run. |
| #181 | AC2 — Refresh is read-only and cannot transition or close a workspace. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records a GET route outside the mutation guard and fake-runtime assertions; external ownership remains authoritative. |
| #181 | AC3 — Status values use dashboard sanitization and never expose credential-bearing remote URLs. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records sanitization and redacted projection tests; external ownership remains authoritative. |
| #181 | AC4 — Idle/stopped workspaces report unavailable without implicitly starting a container. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records zero-runtime-event idle coverage; external ownership remains authoritative. |
| #181 | AC5 — Go fake-client tests cover dirty, ahead/behind, and unavailable cases. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records table-driven projection/parser tests; external ownership remains authoritative. |
| [#182](https://github.com/matt-riley/waffle/issues/182) | AC1 — Connections show GitHub not-configured, configured-but-failing, and healthy states. | externally owned (GitHub merged) | External PR [#220](https://github.com/matt-riley/waffle/pull/220), merge `6806578`; head `feat/181-182-desk-git-github`. | PR #220 records `unconfigured` / `configured` / `healthy` / `stale` states and probe-cache tests. External ownership remains authoritative. |
| #182 | AC2 — Payload excludes app ID, installation ID, private key, token, and base URL. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records Go and Playwright canary assertions; external ownership remains authoritative. |
| #182 | AC3 — Intake watchers show repo, label, and concurrency only. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records sanitized watcher projection; external ownership remains authoritative. |
| #182 | AC4 — Workspace open names the missing GitHub prerequisite before submit when unconfigured. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records the pre-submit dialog behavior; external ownership remains authoritative. |
| #182 | AC5 — Go tests cover all GitHub states and serialized-payload secret exclusion. | externally owned (GitHub merged) | Same external PR #220 / merge `6806578`. | PR #220 records `go test -race ./...`, 66/66 client tests, and 21 browser tests; external ownership remains authoritative. |
| [#183](https://github.com/matt-riley/waffle/issues/183) | AC1 — Today starts a new conversation without URL editing. | merged | PR [#217](https://github.com/matt-riley/waffle/pull/217), merge `9d1bdbd`; final local commits `ab7649f`, `4f5308c`, `f13793d`. | PR #217 records 60 client tests, full race suite, and 21 browser tests passed (51 skipped by fixture policy). |
| #183 | AC2 — Recent sessions show title/last-updated and selecting one resumes in place. | merged | Same PR/commit set as AC1. | PR #217 records new/resume and session switching; `node --test internal/dashboard/ui/today_client_test.mjs` 29/29 passed. |
| #183 | AC3 — Usage, permissions, and working set are viewable from Today. | merged | Same PR/commit set as AC1. | PR #217 summary records usage, permissions, and workset panels; full checks passed. |
| #183 | AC4 — Model errors display and sandbox mode appears in context. | merged | Same PR/commit set as AC1. | PR #217 summary records model errors and sandbox state; Go/dashboard tests passed. |
| #183 | AC5 — Client tests cover new-conversation and resume paths against the fake endpoint. | merged | Same PR/commit set as AC1. | `node --test internal/dashboard/ui/today_client_test.mjs` — 29/29 passed, recorded in PR #217. |
| [#185](https://github.com/matt-riley/waffle/issues/185) | AC1 — Fenced code, inline code, lists, and headings render structurally, with one-action code copy. | merged | PR [#217](https://github.com/matt-riley/waffle/pull/217), merge `9d1bdbd`; final commit `f13793d`. | PR #217 records safe Markdown/DOM rendering and 29/29 Today client tests. |
| #185 | AC2 — Model HTML/script strings never become markup; client test proves inert rendering. | merged | Same PR/commit set as AC1. | PR #217 records inert DOM rendering; full race suite and browser checks passed. |
| #185 | AC3 — Ctrl/Cmd+Enter sends, Enter remains newline, and the hint matches. | merged | Same PR/commit set as AC1. | PR #217 summary records Cmd/Ctrl+Enter behavior; Today client suite passed. |
| #185 | AC4 — Tool rows pair start/finish, show duration, and distinguish success/failure. | merged | Same PR/commit set as AC1. | PR #217 summary records paired tool-call evidence; Go/client/browser gates passed. |
| #185 | AC5 — Transcript rendering remains within the existing sanitization/redaction boundary. | merged | Same PR/commit set as AC1. | PR #217 records full race suite and browser verification; no remaining gate recorded. |
| [#188](https://github.com/matt-riley/waffle/issues/188) | AC1 — Model alias removal requires explicit replacement for role/session references and never substitutes silently. | open/pending | Local branch `codex/issues-removal`; implementation commits `2087ec2`, `0f57ee5`; **no PR currently exists**. Update placeholder: add final verified commit and PR URL here after review. | Worker report recorded focused `go test -race ./internal/dashboard ./internal/providerconfig ./internal/session ./cmd/waffle -count=1`; review found an Important atomicity gap between session replacement and provider removal. Not complete. |
| #188 | AC2 — Provider removal works and gives a specific referenced-alias refusal. | open/pending | Same local branch/commits; no GitHub PR. Update placeholder: replace with final PR/merge or open-PR evidence. | Worker report recorded `node --test internal/dashboard/ui/capabilities_client_test.mjs` (18 passed); final branch/review verification is still required. |
| #188 | AC3 — Both removals use preview/confirm and name the removal/references. | open/pending | Same local branch/commits; no GitHub PR. | Focused tests were reported, but branch completion and final review have not been verified. |
| #188 | AC4 — Both removals write `policy_audit` rows. | open/pending | Same local branch/commits; no GitHub PR. | Update placeholder after final verification with exact audit test command/result and PR URL. |
| #188 | AC5 — Go tests cover referenced refusal, replacement, and success. | open/pending | Same local branch/commits; no GitHub PR. | The latest review identified the cross-operation failure window; do not mark complete until the fix and re-review pass. |
| [#190](https://github.com/matt-riley/waffle/issues/190) | Task 7 requirement — Reconcile all original 17 issues at requirement level with direct implementation/commit/PR references and current state. | open/pending | Prior audit PR [#191](https://github.com/matt-riley/waffle/pull/191), merge `abc6942`; this artifact is the reconciliation document on branch `codex/issues-audit`. Update placeholder: add the artifact PR URL if/when this branch is published. | This commit adds the matrix. #190 itself remains open; publication/branch-PR evidence is outside this artifact-only request. |
| #190 | Task 7 requirement — Record exact verification results and known external/pending gates without inferring completion from issue closure or indirect coverage. | open/pending | `docs/dashboard-ux-audit.md`, task-7 brief/recon, and this matrix. | The matrix explicitly preserves external ownership and keeps #188/#195 pending. Final #190 closure still needs review and publication evidence. |
| [#192](https://github.com/matt-riley/waffle/issues/192) | AC1 — Desk reports configured, missing, and misconfigured setup prerequisites. | externally owned (GitHub open) | External worktree `codex/issues-setup`; no current GitHub PR found by `gh pr list --head codex/issues-setup`. | No implementation/verification claim is made here. External owner must supply the final branch/PR evidence. |
| #192 | AC2 — Satisfiable prerequisites offer safe inline actions; others name the exact CLI command. | externally owned (GitHub open) | External worktree `codex/issues-setup`; no current PR. | Pending external evidence; no browser or application verification claimed. |
| #192 | AC3 — `waffle setup` offers dashboard enablement and prints the loopback URL. | externally owned (GitHub open) | External worktree `codex/issues-setup`; no current PR. | Pending external evidence; no completion claim. |
| #192 | AC4 — No key material, private key, or API key is returned to the browser. | externally owned (GitHub open) | External worktree `codex/issues-setup`; no current PR. | Pending external evidence; no completion claim. |
| #192 | AC5 — Go tests cover fresh, partial, and fully configured prerequisite projections. | externally owned (GitHub open) | External worktree `codex/issues-setup`; no current PR. | Pending external evidence; no completion claim. |
| [#193](https://github.com/matt-riley/waffle/issues/193) | AC1 — Current-session effective prompt is viewable with inline/file source labelled. | externally owned (GitHub merged) | External PR [#221](https://github.com/matt-riley/waffle/pull/221), merge `94dbc01`; head `feat/193-desk-posture-view`. | PR #221 records inline, `@`-path, bare `.md`, and in-home absolute-path tests. External ownership remains authoritative. |
| #193 | AC2 — Resolved tool policy shows group, profile, and repo layers. | externally owned (GitHub merged) | Same external PR #221 / merge `94dbc01`. | PR #221 records layered projection and redaction tests; external ownership remains authoritative. |
| #193 | AC3 — Denied tool calls can be traced to the denying rule. | externally owned (GitHub merged) | Same external PR #221 / merge `94dbc01`. | PR #221 records the policy-denials endpoint and tests; external ownership remains authoritative. |
| #193 | AC4 — View is read-only, redacted, and exposes no filesystem path outside the labelled prompt source. | externally owned (GitHub merged) | Same external PR #221 / merge `94dbc01`. | PR #221 records GET-only routes, 405 mutation responses, and redaction tests; external ownership remains authoritative. |
| #193 | AC5 — Go tests cover inline/file prompt resolution and layered policy projection. | externally owned (GitHub merged) | Same external PR #221 / merge `94dbc01`. | PR #221 records `go test -race ./...`, 76/76 client tests, and 22 browser tests; external ownership remains authoritative. |
| [#194](https://github.com/matt-riley/waffle/issues/194) | AC1 — Structured editor creates, edits, copies, and deletes profiles without raw TOML. | externally owned (GitHub merged) | External PR [#223](https://github.com/matt-riley/waffle/pull/223), merge `a740014`; head `feat/194-desk-profile-editor`. | PR #223 records `DisallowUnknownFields` and raw-TOML rejection; external ownership remains authoritative. |
| #194 | AC2 — Widening tool/sandbox policy is refused by the runtime's own narrowing validator with a specific field. | externally owned (GitHub merged) | Same external PR #223 / merge `a740014`; builds on external PR #221. | PR #223 records `config.ValidateProfileNarrows` and per-field refusal tests; external ownership remains authoritative. |
| #194 | AC3 — Changes preview the resolved before/after posture and require confirmation. | externally owned (GitHub merged) | Same external PR #223 / merge `a740014`. | PR #223 records candidate-bound preview tokens and tamper/review tests; external ownership remains authoritative. |
| #194 | AC4 — Referenced profile deletion is refused with named references. | externally owned (GitHub merged) | Same external PR #223 / merge `a740014`. | PR #223 records config/runtime reference checks; external ownership remains authoritative. |
| #194 | AC5 — Every mutation is audited and restart-deferred. | externally owned (GitHub merged) | Same external PR #223 / merge `a740014`. | PR #223 records the shared mutation wrapper, `policy_audit`, and `CommitForRestart`; external ownership remains authoritative. |
| #194 | AC6 — Tests cover narrowing refusal, referenced deletion, and config round-trip preservation. | externally owned (GitHub merged) | Same external PR #223 / merge `a740014`. | PR #223 records `go test -race ./...`, 82/82 client tests, 22 browser tests, and round-trip assertions; external ownership remains authoritative. |
| [#195](https://github.com/matt-riley/waffle/issues/195) | AC1 — Capabilities, Tasks, Workspaces, and Memory use server-rendered templ fragments and delete corresponding hand-written fetch/render JS; Today stays bespoke. | open/pending | Local branch `codex/issues-htmx` has uncommitted changes (`internal/dashboard/fragments.go`, `htmx.min.js`, fragment templates/handlers); **no commit or PR currently exists**. Update placeholder: add final commit, PR URL, and verified section list. | No branch verification is claimed. The current worktree is dirty and the issue remains open. |
| #195 | AC2 — CSP is byte-for-byte unchanged; no `hx-on:`, `js:` prefix, or eval-dependent extension appears. | open/pending | Same uncommitted branch; no commit/PR. | Update placeholder after a diff check against `internal/dashboard/security.go` and a repository search for forbidden htmx forms. |
| #195 | AC3 — htmx is vendored, embedded, version-pinned, digest-recorded, and makes no off-origin request. | open/pending | Same uncommitted branch; `internal/dashboard/ui/assets/htmx.min.js` is present but not committed or verified here. | Add exact version/digest and browser/network result after final branch verification. |
| #195 | AC4 — Mutations preserve desk token, fresh idempotency key, audit, and unchanged-submission key reuse. | open/pending | Same uncommitted branch; no PR. | Add exact mutation/client test commands and results after verification; do not infer from existing JSON paths. |
| #195 | AC5 — JSON APIs remain available and tested for every migrated endpoint. | open/pending | Same uncommitted branch; no PR. | Add per-section JSON compatibility results after verification. |
| #195 | AC6 — `today.js` is untouched by the htmx migration. | open/pending | Same uncommitted branch; no PR. | Add `git diff -- today.js` result after final commit; current branch is not complete. |
| #195 | AC7 — Dashboard browser gate and per-section client suites exercise real handlers/assets. | open/pending | Same uncommitted branch; no PR. | Add exact `mise run dashboard-check` and client-suite results after the branch is committed, reviewed, and published. |
| [#216](https://github.com/matt-riley/waffle/issues/216) | R1 — Returning to Today must reattach or safely recover the same owner's session while preserving one-owner enforcement. | merged | PR [#217](https://github.com/matt-riley/waffle/pull/217), merge `9d1bdbd`; final commits `4f5308c`, `f13793d`. | PR #217 records rotating reattach leases, bounded recovery, and focused race tests; full race suite passed. |
| #216 | R2 — Foreign clients cannot take over the active session. | merged | Same PR/commit set as R1. | PR #217 summary records preserved single-owner enforcement; `go test -race ./internal/dashboard ./cmd/waffle` passed. |
| #216 | R3 — Navigation/pagehide/reload paths release or recover ownership. | merged | Same PR/commit set as R1. | PR #217 records stale pagehide rejection and browser reload/navigation recovery; 21 browser tests passed. |
| #216 | R4 — Stale callbacks and stale recovery proofs cannot mutate the new owner. | merged | Same PR/commit set as R1. | PR #217 records bounded 30-second recovery proofs and final review fix; focused race tests passed. |
| #216 | R5 — Crashes remain recoverable through a bounded TTL backstop. | merged | Same PR/commit set as R1. | PR #217 records bounded lost-response recovery and the 30-second lease window; full race/evaluation gates passed. |
| #216 | R6 — Ownership, recovery, and race behavior have focused coverage. | merged | Same PR/commit set as R1. | PR #217 records `go test -race ./internal/dashboard ./cmd/waffle` and full `mise run test` passed. |

Issue #216 has no formal checkbox acceptance section; its rows above are the
behavioral requirements stated in the issue's mechanism, suggested directions,
and concurrency notes, as mapped by the Task 7 reconnaissance.

## Evidence sources and scope notes

Primary sources used for this reconciliation:

- `AGENTS.md` — repository verification, commit, and documentation constraints.
- `docs/dashboard-ux-audit.md` — the original Desk findings and the #195
  architectural direction; merged audit source PR [#191](https://github.com/matt-riley/waffle/pull/191).
- `docs/superpowers/plans/2026-07-25-waffle-open-issues.md` — issue grouping,
  exclusions, and Task 7 acceptance evidence.
- `.superpowers/sdd/2026-07-25-waffle-open-issues/task-7-brief.md` and
  `task-7-recon.md` — the requirement-level evidence contract and prior
  reconnaissance. These are workspace evidence files, not application changes.
- Read-only GitHub commands: `gh issue list`, `gh issue view`, `gh pr list`,
  `gh pr view`, and `gh auth status`, all against `matt-riley/waffle`.

The six previously merged prerequisite fixes (#179, #180, #184, #186, #187,
and #189) are not rows in the original 17-item matrix. Their PRs remain
supporting evidence only; related coverage is not used to infer completion of
another issue.

The artifact intentionally does not modify application code or any other
documentation. It also does not close issues, create a PR, or change the
externally owned branches.
