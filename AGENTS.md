# Repository Guidelines

## Project Structure & Module Organization

This is a Go OpenAI-compatible routing gateway. Single module `github.com/nanorouter/nanorouter`; executable entry point is `cmd/nanorouter/main.go`. All core logic (routing, upstream adaptation, usage parsing, SQLite/PostgreSQL persistence, request sanitization, backpressure) lives in one package, `internal/router/`, so `go test ./...` is effectively a single package. Tests sit beside implementation as `internal/router/*_test.go`. Keep user-facing setup in `README.md`, architectural decisions in `docs/design.md`, and runnable provider examples in `config.example.yaml`.

## Design Reference

Read [docs/design.md](docs/design.md) before changing routing, upstream protocol adaptation, retry or
backpressure behavior, persistence, observability, deployment boundaries, or security assumptions. Keep
that document updated when a change alters the architecture or one of its explicit trade-offs; avoid
duplicating the full design rationale in this file.

## Build, Test, and Development Commands

- `go run ./cmd/nanorouter -config config.yaml` — run the gateway locally; `-debug` forces colored text logs at DEBUG level. `make build` (outputs `bin/nanorouter`) and `make run` are equivalent shortcuts.
- `go test ./...` — run all unit tests; execute before every handoff. Focused runs: `go test ./internal/router` or `go test ./internal/router -run '^Test<Name>$'`. Perf work uses the benchmarks in `internal/router/benchmark_test.go` or `scripts/bench.sh` / `scripts/profile.sh`.
- `go vet ./...` — detect common Go correctness issues.
- `go build -buildvcs=false ./cmd/nanorouter` — build without requiring Git metadata; use this in this workspace (the Dockerfile also builds this way).
- `gofmt -w cmd internal` — format changed Go files before testing.

Copy `config.example.yaml` to `config.yaml`; both are gitignored along with `nanorouter.db` and `bin/` — never commit real API keys, the generated database, or build artifacts.

## Coding Style & Naming Conventions

Use standard Go formatting and package names in lowercase. Prefer small, focused functions and explicit error handling. Keep provider-specific transformations in `internal/router/` rather than the HTTP entry point. Use `goccy/go-json` (`json` import alias) for JSON handling, `zap` for structured logs, and avoid logging prompts, completions, authorization headers, or keys. Add short Chinese comments for non-obvious routing, fallback, streaming, or compatibility decisions (docs and config comments are in Chinese too).

## Testing Guidelines

Use the standard `testing` package with `httptest` and in-memory SQLite (`sql.Open("sqlite3", ":memory:")`). Name tests `Test<Behavior>` (for example, `TestCandidateSpecificRequestSanitization`). Cover both success and failure behavior when changing routing: 429 fallback, disabled candidates, request rewriting, SSE usage extraction, and provider-specific adapters. Do not call real upstream APIs in tests.

## Commit & Pull Request Guidelines

Branch is `main` with a Gitea remote (`git@coding.fly97.fun:fly97/nanorouter.git`). Use Conventional Commit-style messages: `feat: add cerebras route`, `fix: preserve stream usage`, or `docs: update provider setup`. Keep commits focused. Pull requests should explain the affected endpoints/providers, configuration changes, fallback or compatibility implications, and verification commands run. Include sanitized request/response examples for protocol changes.

## CI & Container

The Gitea Actions workflow in `.gitea/workflows/build-image.yaml` builds the Docker image on every push and PR; on `main` pushes or manual runs it pushes `:<sha>` and `:latest` to the Gitea container registry. Dockerfile deploys distroless non-root and reads `/etc/nanorouter/config.yaml`.

## Security & Configuration

Use environment variables referenced by `api_key_env`; do not place secrets in YAML. Treat `responses_via_chat_completions` as a limited adapter: unsupported Responses features must fail explicitly rather than being silently dropped.
