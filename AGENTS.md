# AGENTS.md

Project instructions for coding agents working on **mrmr**.

## Source of truth

- Read `IMPLEMENTATION.md`, then inspect the current code and tests before changing anything.
- Conflict order: explicit user instruction → `IMPLEMENTATION.md` milestone behavior → this file.
- The codebase is authoritative for existing structure; do not assume the docs are already implemented.

## Core boundaries

> **LLM = judgment. Runtime = authority.**

Keep the pipeline explicit: Source → Event → Interpreter → Decision → Policy → Outcome → Adapter.

- Sources only detect and normalize events.
- Events are durable before downstream work depends on them.
- Interpreters return validated structured Decisions and perform no side effects.
- Policy is deterministic; conditions ordinary code can evaluate do not belong in prompts.
- Outcomes describe intended effects; adapters perform bounded effects without embedding policy.
- Decisions and effects remain traceable to their parent Event.
- Treat model output as untrusted. Never execute model-generated commands, URLs, tools, permissions, or arguments without deterministic validation.
- Fail toward `ignore`, never toward unauthorized action.

## Scope and design

- Implement only the requested milestone. Do not scaffold future phases.
- Prefer Go, SQLite, HTTP, JSON, YAML, stdlib, and one process/binary.
- Use AI only for ambiguity; use ordinary code for deterministic work.
- Keep the core small. Integrations belong in narrow adapters or recipes.
- No speculative frameworks, plugin systems, provider abstractions, DI containers, custom DSLs, or distributed infrastructure.
- Add an abstraction only after multiple real implementations require it.

## Security and data

- Never log, return, trace, or commit API keys, bearer tokens, or webhook secrets.
- Secrets come from environment variables or an approved secret store, never flow definitions.
- Event payloads may be sensitive. Avoid persisting prompts or raw external responses unless explicitly required.
- Model output is validated, retries are bounded, and a model cannot expand its own authority.

## Engineering

- Go: use `context.Context`, explicit wrapped errors, small interfaces at the point of use, and `gofmt`.
- SQLite migrations are additive, forward-safe, transactional, and tested. Never silently destroy data.
- Do not hold transactions across model calls, HTTP requests, notifications, or agent execution.
- Preserve completed API behavior; prefer additive changes and document incompatibilities.
- Validate configuration before activation. Invalid config must not replace the last known valid state.
- Prefer stdlib or existing dependencies. Add dependencies only for clear, current value.
- Use domain names consistently: Source, Event, Interpreter, Decision, Policy, Outcome, Adapter, Execution, Connection, Flow.
- All substantive new code must follow Mitchell Hashimoto’s comment style: comment heavily and explain why—constraints, invariants, ownership, edge cases, and non-obvious control flow—not merely what the syntax does.
- Goroutines require ownership, bounded lifetime, shutdown, and leak-free tests.

## Tests and completion

- Test changed behavior at the smallest useful level; use integration tests for SQLite and HTTP boundaries.
- Tests must not require internet access, real credentials, external services, or developer-specific paths.
- Before completion, run the relevant checks. Go minimum: `gofmt`, `go test ./...`, and `go build ./...`.
- Review for scope creep, secret leakage, API breakage, missing errors/tests, and unnecessary abstraction.
- Report what changed, validation run, intentional deviations, and anything remaining.
