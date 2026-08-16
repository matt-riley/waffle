# Repository Guidelines

## Project Structure & Module Organization

Waffle is a Go application; `mise.toml` pins the toolchain (currently 1.26.5) and `go.mod` declares the language version (currently 1.25.13). The executable and command handlers live in `cmd/waffle/`; core behavior is organized into focused packages under `internal/`, including `agent`, `gateway`, `sandbox`, `store`, and providers in `internal/llm/`. Keep Go tests beside their implementation as `*_test.go`. Ordered SQL migrations belong in `internal/store/migrations/`, evaluation scenarios in `evals/`, and architecture or operations documentation in `docs/`. Brand source assets live in `assets/brand/`, with supporting scripts and tests in `tools/brand-assets/`. Use `config.example.toml` as the configuration reference.

## Build, Test, and Development Commands

Use mise to install the pinned toolchain and run standard tasks:

- `mise install` installs Go and other pinned tools.
- `mise run build` creates the version-stamped `bin/waffle` binary.
- `mise run test` regenerates templ components, fails if `internal/dashboard/ui/*_templ.go` is dirty, runs the Waffle Desk client tests, then `go test -race ./...` and the zero-network `waffle eval`.
- `mise run fmt`, `mise run vet`, and `mise run lint` check formatting, static analysis, and lint rules.
- `mise run website-check` builds the website and runs its tests; `mise run docs-screenshots` regenerates the Waffle Desk screenshots used in the documentation site.
- `mise run brand-check` tests and validates brand raster assets and manifests.

For focused iteration, run `go test ./internal/agent -run TestName`. Run locally with `go run ./cmd/waffle chat` after configuring the required provider and secrets. Sandbox-specific checks are documented in `docs/sandbox-queue.md`.

## Coding Style & Naming Conventions

Format every Go change with `gofmt`; accept its tab indentation. Use short lowercase package names, `PascalCase` for exported identifiers, and `camelCase` for unexported identifiers. Keep package boundaries aligned with responsibilities, wrap errors with context using `%w`, and document any necessary `nolint` suppression.

## Testing Guidelines

Use Go's `testing` package and table-driven tests where cases share behavior. Name tests `TestBehavior` and add regression coverage for fixes, including relevant failure, cancellation, persistence, and concurrency paths. Before submitting, run `mise run test`, `mise run fmt`, `mise run vet`, and `mise run lint`. Live provider evaluations require `WAFFLE_EVAL_LIVE=1` and valid provider configuration.

## Commit & Pull Request Guidelines

Use Conventional Commits with focused, imperative subjects, such as `feat: add workspace cleanup`, `fix: handle cancelled runs`, `docs: clarify deployment`, or `build(deps): bump sqlite`. Issue-scoped forms such as `fix(#68): ...` are also accepted. Pull requests should explain behavior changes, link issues, list verification commands, and call out configuration or migration effects. Include screenshots only for user-visible output changes.

## Security & Configuration

Never commit API keys, identities, generated databases, or local `config.toml`. Preserve deny-by-default sandbox and network policies. Treat migrations, secret-store changes, and policy relaxations as high risk; document compatibility and rollback considerations.
