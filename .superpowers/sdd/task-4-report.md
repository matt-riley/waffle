# Task 4: Self-development acceptance gaps

## Outcome

Implemented acceptance criteria #62.1, #62.3, #62.6, #62.7, and #63.5 without weakening their wording.

## RED/GREEN evidence

- RED: focused selfdev tests initially failed to compile because `upgradeInto`, `Reviewer`, `Finding`, `ReviewRecord`, and `enforceReview` did not exist.
- GREEN: `TestUpgradeIntoFailingTestShowsOutputAndDoesNotSwap` runs a real Go fixture with one failing test, observes the failure text, and proves the target binary is unchanged.
- GREEN: `TestUpgradeIntoBrokenEvalShowsOutputAndDoesNotSwap` runs the verify ladder against a deterministic failing eval and proves the target binary is unchanged.
- GREEN: `TestVerifyReportsEachFailingMechanicalGate` proves vet, race-test, and installed-lint failures expose their command output and stop verification.
- GREEN: `TestVerifyMissingLintWarnsWithoutFailing` and `TestDoctorReportsLintGateUnarmed` prove optional-lint degradation and doctor reporting.
- GREEN: `TestReviewerUsesUtilityModelAndReturnsStructuredFindings` pins structured JSON findings and the exact provider request model.
- GREEN: `TestReviewGatePersistsSHAAndBlocksManualAndAutoPatch` proves blocker findings stop both required policies after the SHA and findings are durably appended.
- GREEN: `TestUpgradeReviewerBlockerStopsManualAndAutoPatch` drives the production upgrade entry point for both policies, captures the configured utility model, reads the persisted candidate SHA, and proves blocker review occurs before checkout.
- GREEN: `TestReviewerModelUsesConfiguredUtilityModel` proves configured utility selection wins over the generation model.

## Implementation

- Extracted `upgradeInto` so the verify/build/doctor/install boundary can be exercised safely against a temporary target while production still replaces `os.Executable()`.
- Added a structured `Reviewer` and strict `Finding` validation (`blocker`, `warn`, `nit`; non-empty file and summary).
- Wired review before candidate checkout. The reviewer examines the candidate diff, resolves the candidate commit SHA, selects `[provider].utility_model` when configured, and falls back to the primary model.
- Added append-only `$WAFFLE_HOME/selfdev-reviews.jsonl` records containing commit SHA, approval mode, timestamp, and findings. Persistence occurs before blocker enforcement.
- Documented reviewer behavior in `docs/plan.md` and replaced the five generic audit rows with exact symbols, tests, and commands.

## Verification

- `go test ./internal/selfdev ./internal/config ./cmd/waffle -count=1` — PASS.
- `go test -race ./internal/selfdev -count=1` — PASS.
- `go test ./... -count=1` — PASS.
- `mise run build` — PASS.
- `go vet ./...` — PASS.
- `go run ./cmd/waffle eval` — PASS, 11/11 deterministic evals.
- `mise run fmt` — PASS.
- `git diff --check` — PASS.
- `mise run lint` — FAILS on six pre-existing/unrelated `errcheck` findings in `internal/backup/backup_test.go`, `internal/instance/instance.go`, `internal/instance/instance_test.go`, and `internal/usage/usage_test.go`. No lint finding points to Task 4 files.

## Configuration and compatibility

- No schema or database migration is required; review records are JSONL in the existing Waffle home directory.
- Existing upgrade function signatures remain stable.
- `--no-verify` remains an emergency mechanical-gate bypass and deliberately does not bypass review.
