# All-Issues Acceptance Audit Design

## Goal

Prove that every acceptance criterion in all 64 closed GitHub issues (#8 through #79, excluding numbers for which no issue exists) is genuinely satisfied, repair every gap, and leave criterion-level evidence that can be rechecked.

## Source of truth

The issue body is authoritative for acceptance criteria and explicit completion conditions. A checked checkbox, a closed issue, an implementation commit, or documentation alone is a claim rather than proof.

Each explicit checkbox and each unambiguous prose completion condition will become one row in a version-controlled audit matrix. Criteria that describe alternative terminal states or deliberate scope decisions will be evaluated against the selected outcome recorded in the repository.

## Evidence model

Each criterion receives exactly one status:

- `pass`: the required implementation or artifact exists and is supported by current verification evidence.
- `partial`: some required behavior or evidence exists, but at least one part of the criterion is unsupported.
- `fail`: the required behavior or artifact is absent, contradicted, or fails verification.
- `not-applicable`: the criterion belongs to an explicitly unselected alternative terminal state, with the selected state documented and justified.

A passing row identifies:

1. the implementation, document, migration, command, or decision artifact;
2. the automated test or deterministic verification that exercises it;
3. the verification command and current result;
4. manual evidence only when the criterion depends on unavailable infrastructure.

Infrastructure-dependent criteria may pass through deterministic gated tests plus documented manual evidence. Examples include Docker Desktop behavior, live provider reachability, GitHub App credentials, and platform-specific service operation. A skipped gated test by itself is insufficient: its setup, assertions, invocation, and expected successful result must be inspectable, and the manual procedure must be documented.

## Audit workflow

First, export all issue bodies and normalize their acceptance criteria into the audit matrix without trusting checkbox state. Map each row to code, tests, documentation, migrations, configuration examples, or decision records. Review issue dependencies without merging separate criteria merely because one implementation serves several issues.

Then verify the evidence. Run focused tests for each subsystem and inspect whether assertions prove the entire criterion rather than merely execute nearby code. Security criteria require negative or bypass cases at the enforcement boundary. Persistence criteria require store round trips or migration coverage. Lifecycle and concurrency criteria require state-transition or synchronization evidence. Documentation and decision criteria require the promised artifact and consistency with current behavior.

Every `partial` or `fail` row enters remediation. For behavioral gaps, add a focused regression test that fails for the missing behavior before changing implementation. For evidence-only gaps, strengthen the test or documentation without changing unrelated behavior. Avoid broad refactors unless a criterion cannot be met safely within existing boundaries.

After remediation, rerun focused verification and update the matrix with stable file and test references. Complete verification includes the repository's normal test command, Go tests, race tests, vet, lint when installed, deterministic evals, build, and any tagged deterministic tests that do not require unavailable infrastructure. Gated infrastructure tests are compiled and inspected even when their external dependency is absent.

## Deliverables

- A version-controlled criterion-level audit matrix covering every in-scope issue.
- Focused regression tests and minimal implementation changes for every discovered gap.
- Updated operational documentation where gated/manual evidence is required.
- A final per-issue report summarizing pass, partial, fail, and not-applicable counts and naming any criterion that cannot honestly pass.

## Constraints

- All 64 closed issues are in scope, not only the recent backlog.
- Closed or checked status never substitutes for evidence.
- Existing user changes are preserved.
- No criterion is weakened or reworded merely to obtain a pass.
- No external service credentials are required when deterministic gated tests and documented manual evidence provide equivalent reviewable evidence.
- The audit does not create, close, or edit GitHub issues unless separately requested.

## Completion condition

The work is complete only when every criterion is recorded in the matrix, every row is supported by current evidence, and every row is `pass` or a justified `not-applicable`. Any remaining `partial` or `fail` is reported as unfinished work rather than described as complete.
