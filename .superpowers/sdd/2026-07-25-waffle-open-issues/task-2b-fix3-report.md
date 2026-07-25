# Waffle open issues: task 2b fix 3

Date: 2026-07-25
Base commit: `79b5b10`
Scope: lifecycle hardening for #176, #177, and #178 only.

## Changes

- Attachment SQL now trims at insert, comparison, delete, and read boundaries. `List` and `References` emit distinct canonical values, so legacy rows such as ` reviewer ` remain discoverable, idempotent, and removable without duplicate output.
- Standalone `skills ls`, `activate`, and `deactivate` commands acquire the shared lifecycle guard before pending-journal recovery and hold it through the follow-up operation. The locked recovery entry point avoids recursive guard acquisition.
- Store sidecar locks now use an absolute, symlink-resolved database path after migration, with an absolute cleaned fallback when resolution is unavailable. Relative and symlinked openings of the same SQLite state therefore share the lock.
- Journal recovery now uses `Lstat` and accepts only real directories for visible and staged skill paths. Dangling symlinks, files, and other ambiguous states fail closed and leave the journal available for inspection. Frontmatter names that differ from directory names remain supported.

## Regression coverage

- SQL-seeded legacy whitespace and duplicate attachment rows.
- Guard ordering across recovery and a follow-up activation operation.
- Two store openings through a database symlink.
- Prepared and committed journals containing files or dangling symlinks.
- Existing recovery coverage for a frontmatter-name/directory-name mismatch remains green.

## Verification

- `go test -race ./internal/skill ./internal/store ./cmd/waffle -count=1`
- `go test ./internal/skill ./internal/store ./cmd/waffle -count=1`
- `mise run test` — all Go packages, dashboard client tests, and 11/11 evaluations passed.
- `mise run fmt`
- `mise run vet`
- `mise run lint` — 0 issues.
- `git diff --check`

No push or pull request was created.
