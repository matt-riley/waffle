# Repository Guidelines

## Project Structure & Module Organization

Waffle is a Go 1.25 application. The executable and command handlers live in `cmd/waffle/`. Core behavior is split into focused packages under `internal/`, such as `agent`, `gateway`, `sandbox`, `store`, and provider implementations in `internal/llm/`. Keep tests beside the code they exercise as `*_test.go`. Database migrations are ordered SQL files in `internal/store/migrations/`. Evaluation scenarios live in `evals/`; architecture, deployment, and operational notes live in `docs/`. Use `config.example.toml` as the configuration reference.

## Build, Test, and Development Commands

The repository uses mise to pin Go and provide standard tasks:

- `mise install` installs the pinned Go toolchain.
- `mise run build` builds the version-stamped binary at `bin/waffle`.
- `mise run test` runs all packages with the race detector, then runs zero-network evaluations.
- `mise run vet` runs `go vet ./...`.
- `mise run fmt` verifies that all Go files are formatted.
- `mise run lint` runs `golangci-lint` across the repository.

For focused work, use `go test ./internal/agent -run TestName`. Sandbox-specific tests are documented in `docs/sandbox-queue.md`.

## Coding Style & Naming Conventions

Run `gofmt` on every Go change; use tabs as emitted by the formatter. Follow standard Go naming: short lowercase package names, exported identifiers in `PascalCase`, and unexported identifiers in `camelCase`. Keep package boundaries aligned with responsibilities and wrap errors with context using `%w`. Avoid `nolint` suppressions unless the reason is documented.

## Testing Guidelines

Use Go's `testing` package, favoring table-driven tests for multiple cases. Name tests `TestBehavior` and place them beside the implementation. Add regression tests for bug fixes and cover failure paths, cancellation, persistence, and concurrency where relevant. Run the focused package test while iterating, then `mise run test`, `mise run vet`, and `mise run lint` before opening a pull request.

## Commit & Pull Request Guidelines

History follows Conventional Commit-style subjects such as `feat:`, `fix:`, `docs:`, and `build(deps):`; issue-scoped forms like `fix(#68): ...` are also used. Keep commits focused and write imperative, specific subjects. Pull requests should explain the behavior change, link relevant issues, list verification commands, and note configuration or migration effects. Include screenshots only when terminal or user-visible output changes.

## Security & Configuration

Never commit API keys, identities, generated databases, or local `config.toml`. Preserve deny-by-default sandbox and network policies, and document any deliberate relaxation. Treat migrations and secret-store changes as high risk and include rollback or compatibility notes.
