# Repository Guidelines

## Project Structure & Module Organization

This is a Go OpenAI-compatible routing gateway. The executable entry point is `cmd/nanorouter/main.go`. Core routing, upstream adaptation, usage parsing, SQLite persistence, and request sanitization live in `internal/router/`. Tests sit beside implementation in `internal/router/*_test.go`. Keep user-facing setup in `README.md`, architectural decisions in `docs/design.md`, and runnable provider examples in `config.example.yaml`.

## Build, Test, and Development Commands

- `go run ./cmd/nanorouter -config config.yaml` — run the gateway locally.
- `go test ./...` — run all unit tests; execute before every handoff.
- `go vet ./...` — detect common Go correctness issues.
- `go build -buildvcs=false ./cmd/nanorouter` — build without requiring Git metadata; use this in this workspace.
- `gofmt -w cmd internal` — format changed Go files before testing.

Copy `config.example.yaml` to `config.yaml`; never commit real API keys or the generated `nanorouter.db`.

## Coding Style & Naming Conventions

Use standard Go formatting and package names in lowercase. Prefer small, focused functions and explicit error handling. Keep provider-specific transformations in `internal/router/` rather than the HTTP entry point. Use `goccy/go-json` (`json` import alias) for JSON handling, `zap` for structured logs, and avoid logging prompts, completions, authorization headers, or keys. Add short Chinese comments for non-obvious routing, fallback, streaming, or compatibility decisions.

## Testing Guidelines

Use the standard `testing` package with `httptest` and in-memory SQLite. Name tests `Test<Behavior>` (for example, `TestCandidateSpecificRequestSanitization`). Cover both success and failure behavior when changing routing: 429 fallback, disabled candidates, request rewriting, SSE usage extraction, and provider-specific adapters. Do not call real upstream APIs in tests.

## Commit & Pull Request Guidelines

No Git history is present in this checkout, so use Conventional Commit-style messages: `feat: add cerebras route`, `fix: preserve stream usage`, or `docs: update provider setup`. Keep commits focused. Pull requests should explain the affected endpoints/providers, configuration changes, fallback or compatibility implications, and verification commands run. Include sanitized request/response examples for protocol changes.

## Security & Configuration

Use environment variables referenced by `api_key_env`; do not place secrets in YAML. Treat `responses_via_chat_completions` as a limited adapter: unsupported Responses features must fail explicitly rather than being silently dropped.
