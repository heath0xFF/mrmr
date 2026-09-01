# AGENTS.md

> Project instructions for coding agents working on **mrmr**.
> Agent-agnostic: applies to Codex, Claude Code, Pi/OMP, or any other automated system.

## Project purpose

mrmr is a local-first, event-driven runtime for ambient AI. It connects outside-world events to lightweight AI interpretation, deterministic policy, and controlled outcomes.

Execution model:

```text
                 ┌→ ignore
                 ├→ notify
event → interpret├→ act
                 └→ delegate
```

Invariant:

> **LLM = judgment. Runtime = authority.**

Models may classify, summarize, score, recommend, or interpret. Models must not bypass runtime policy, permission, or execution boundaries.

## Reading order and conflict resolution

Before changes: read `IMPLEMENTATION.md`, then this file, then inspect the current repo. (`mrmr.md` is referenced historically; treat the codebase and `IMPLEMENTATION.md` as source of truth.)

On conflict: explicit user instruction → `IMPLEMENTATION.md` (milestone behavior) → this file (engineering behavior).

The current codebase is the source of truth for existing structure. Do not assume it matches the docs.

## Core architecture boundaries

Each stage has one job. Keep them separated.

- **Sources** detect events and emit normalized **Events**. They do not interpret, apply policy, invoke agents, or cause unrelated side effects.
- **Events** are normalized records of what happened, durable before downstream work depends on them. No source-specific logic in the core Event model.
- **Interpreters** use models to convert Events into structured **Decisions**. They return validated structured output and perform no side effects (no notifications, no adapter calls, no permission grants).
- **Decisions** are validated, persisted where required, traceable to their parent Event, and safe for deterministic policy. No unstructured model prose where machine-readable behavior is required downstream.
- **Policy** is deterministic and selects the next Outcome. Never hide policy in prompts. If ordinary code can evaluate a condition, express it in code, not in a model.
- **Outcomes** describe intended effects. **Adapters** implement effects. Keep selection separate from execution.
- **Adapters** perform bounded external effects (Discord, Telegram, HTTP, CLI, GitHub, delegation). Narrow interfaces, validate config, explicit errors, no embedded policy.

## Design principles

- **Boring infrastructure.** Go, SQLite, HTTP, JSON, YAML, stdlib, single process/binary. No Kafka/K8s/Redis/NATS/service meshes/multi-DB/distributed locks unless a milestone requires it.
- **AI only where ambiguity exists.** If an `if`, parser, lookup, or rule suffices, don't use a model.
- **Keep the core small.** Not a workflow engine, agent framework, MCP framework, inference server, chatbot, plugin marketplace, or AI OS. Prefer adapters/recipes over core expansion.
- **Explicit data flow.** Event → Interpreter → Decision → Policy → Outcome → Adapter must be obvious. No hidden control flow, magic registries, or implicit side effects.
- **No speculative abstractions.** No generic provider frameworks, plugin systems, DI containers, abstract factories, custom DSLs, or event buses "for later." Add abstractions only after multiple real implementations demonstrate the need.

## Scope discipline

Implement only the requested scope. Respect the assigned milestone's requirements, acceptance criteria, tests, non-goals, and done-when conditions. Do not scaffold future milestones unless current work requires shared groundwork.

Prohibited scope expansion: adding auth during an HTTP-ingestion milestone; adding Anthropic support during an OpenAI-compatible milestone; adding a node editor during the initial web UI milestone; adding Redis because an in-process queue feels temporary; adding a permissions framework before permissions are assigned.

A small, complete change beats a broad, partial one.

## Security and secrets (non-negotiable)

Never: log API keys, bearer tokens, or webhook secrets; return secrets through the web API; persist secrets in plaintext flow definitions; include secrets in traces.

Event payloads may contain sensitive user data — be cautious. Do not persist full model prompts or external responses unless the design explicitly requires it. Trace metadata favors IDs, model names, latency, policy match, adapter names, and error summaries over raw content.

The UI may show `configured` / `missing` / `healthy` / `error` for secrets, never raw values.

## Model interaction

Treat model output as untrusted input. Always validate structured output. Never execute model-generated shell commands, URLs, tool names, agent permissions, or API arguments without deterministic runtime validation. One model must never expand its own authority. Bound retries; no infinite model retry loops.

## SQLite and schema

- Migrations are explicit, additive, forward-safe. Never silently destroy data. Deterministic and testable.
- Use established ID prefixes (`evt_`, `dec_`, `exe_`). Don't invent new formats for existing domain types.
- JSON storage is for genuinely flexible payloads (e.g. Event `data`). Don't collapse relational data into opaque JSON to avoid schema design.
- Transactions for multi-writes that must succeed or fail together. Never hold a transaction open across model calls, HTTP requests, notifications, or agent execution.

## API compatibility

Completed endpoints are public within the project. Prefer additive evolution. No casual field renames, silent semantic changes, or response-code changes. Document incompatible changes clearly.

## Configuration

Explicit, human-readable, versionable, safe to inspect. YAML for flow config where established; env vars for secrets and simple process settings. No raw secrets in flow files; no hidden undocumented env vars. Validate before activating; invalid new config must not destroy the last known valid runtime state.

## Dependencies

Before adding one: can stdlib do this clearly? Is it well-maintained? Does it materially reduce complexity? Large transitive tree? Central to the current milestone?

Good: SQLite driver, cron parser, ULID/UUIDv7, an already-selected frontend framework. Poor: future flexibility, framework preference, avoiding 20 lines of code, premature abstraction. Don't replace an established dependency without concrete benefit.

## Conventions

**Go backend:** gofmt, idiomatic package names, `context.Context` through all request-scoped work, explicit error returns (wrap with context: `fmt.Errorf("persist event %s: %w", event.ID, err)`), small interfaces defined at point of use, constructor functions. No global mutable state, panic for normal errors, god packages, reflection-heavy frameworks, or unnecessary goroutines. Goroutines need clear ownership, shutdown, error handling, and bounded/daemon lifetime. Don't leak goroutines in tests. Don't duplicate log lines at every layer — return enriched errors upward or log at final handling.

**TypeScript/web UI:** small components, clear API types, plain state management, existing backend contracts. The frontend consumes backend APIs; never reimplement policy, Decision validation, permission logic, or flow semantics in the browser. Backend is authoritative. Prioritize observability, traceability, config clarity, and runtime health over visual novelty. A simple YAML editor beats a complex node canvas.

**Naming:** use domain vocabulary consistently — Source, Event, Interpreter, Decision, Policy, Outcome, Notification, Action, Delegation, Execution, Connection, Flow. Don't alternate Event/Message/Signal, Decision/Judgment/Classification, or Outcome/Route/Result in core APIs.

**Comments:** explain why, constraints, non-obvious behavior, boundaries — not what the code does.

## Testing

Test all changed behavior at the appropriate level: unit (validation, policy, parsing, template rendering, small adapters), integration (SQLite, HTTP APIs, model HTTP clients, notification adapters, runtime processing, source lifecycles — temp DBs and mock HTTP servers), end-to-end only when it adds real cross-boundary confidence.

Tests must not depend on internet, real API creds, real Discord/Telegram/cloud models, or developer-specific paths. Mock external services unless a task explicitly requires manual integration. Clean up temp files, goroutines, servers, and DB handles.

## Observability

Runtime must stay explainable: what Event caused this, what component handled it, what Decision was produced, what policy matched, what Outcome was selected, what adapter ran, what failed. No opaque execution paths. Logs carry event/decision/execution IDs and source/adapter names; avoid verbose payload logging; never log secrets; consistent levels. Update trace behavior when a new stage is added and the milestone supports tracing.

## Anti-patterns (avoid unless explicitly required)

- **Agent-first architecture:** don't spawn a heavyweight agent per event to decide everything. Prefer cheap interpretation → deterministic policy → delegate only when necessary.
- **Prompt-defined authorization:** not "send email if you think it's appropriate." Model recommends → runtime evaluates policy/permissions → runtime executes or requests approval.
- **Integration-specific core logic:** no GitHub branching in the runtime package. The adapter normalizes into an Event; core stays generic.
- **Hidden side effects:** `Interpret()` must not also send a Discord message. Flow is Interpret → Decision → Policy → Notify Outcome → Discord adapter.
- **Premature distributed architecture:** SQLite + in-process worker for local v0.1, not Kafka+Redis+workers.
- **Giant generic interfaces:** small interfaces around actual usage, not a `Provider` with dozens of unrelated methods.

## Non-goals

Unless explicitly assigned, do not turn mrmr into: a hosted SaaS, multi-tenant platform, general chatbot, n8n replacement, Claude/Codex/Hermes/Pi/OMP replacement, MCP implementation, inference server, vector database, general memory system, distributed orchestration platform, plugin marketplace, or mobile app.

## Task workflow

1. **Inspect** — read relevant docs; inspect current code and tests.
2. **Scope** — what's in/out; follow the assigned milestone.
3. **Implement** — smallest coherent change that satisfies the task.
4. **Test** — focused tests while developing, then the broader required checks before stopping.
5. **Review** — scope creep, secret leakage, API breakage, missing error handling, missing tests, unnecessary abstractions, boundary violations.
6. **Document** — update milestone status/docs only when warranted.
7. **Report** — concise completion report.

## Validation before completion

Before reporting work complete, run the relevant checks. Go minimum:

```bash
gofmt
go test ./...
go build ./...
```

Plus any configured linters/static checks. Frontend: typecheck, tests, build, lint. Don't claim success without reporting actual results. Don't claim a milestone complete unless all required criteria pass.

## Completion report

```
Summary   — what changed
Files     — key files created or modified
Validation — tests run, build/typecheck/lint results
Behavior  — acceptance criteria satisfied
Deviations — intentional differences from the plan
Remaining — blockers, follow-up, next milestone
```

If nothing remains, say so directly.
