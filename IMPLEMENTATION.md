# mrmr

> **A local-first, event-driven runtime for ambient AI.**
> Pronounced **“murmur.”**

mrmr is an open-source runtime for connecting the events happening around you to AI-driven interpretation, deterministic policy, notifications, actions, and more capable agents.

The core idea is simple:

```text
                 ┌→ ignore
                 ├→ notify
event → interpret├→ act
                 └→ delegate
```

Most AI systems wait for a human to ask them to do something. mrmr flips that model around.

Instead of:

```text
human → prompt → agent → tools → result
```

mrmr is built around:

```text
event → interpretation → policy → outcome
```

The environment produces events. Cheap, fast models interpret what those events mean. Deterministic policy decides what is allowed to happen. More capable agents wake up only when something actually deserves them.

---

## Project thesis

Modern AI tooling is extremely good at **interactive work**:

- chat
- coding
- research
- tool use
- multi-agent execution

What is still awkward is **background intelligence**.

A new email arrives. A GitHub issue is opened. A meeting moves. A server disappears. An MQTT sensor reports something unusual. A new task is created. A document changes.

Most of these events do not require a heavyweight agent.

They require a lightweight semantic judgment:

- Does this matter?
- What kind of event is this?
- Does a human need to know?
- Can a deterministic action handle it?
- Should a more capable agent be delegated the work?

mrmr exists to provide that layer.

> **Agents should not have to run continuously. Your environment should produce events, cheap models should interpret them, deterministic policy should govern them, and capable agents should wake up only when necessary.**

---

# Core model

mrmr has six core concepts:

```text
Source
Event
Interpreter
Decision
Policy
Outcome
```

The execution path is:

```text
Source
  ↓
Event
  ↓
Filter
  ↓
Interpreter
  ↓
Decision
  ↓
Policy
  ↓
Ignore / Notify / Act / Delegate
```

## 1. Source

A **Source** detects that something happened and emits a normalized event.

Examples:

- generic webhook
- GitHub webhook
- Jira webhook
- Gmail poller
- Google Calendar poller
- RSS poller
- cron
- filesystem watcher
- MQTT subscriber
- Tailscale watcher
- HTTP endpoint
- local process
- custom plugin

Sources should be deliberately dumb.

A Gmail source should not decide whether an email matters. It should only detect new mail and emit an event.

```text
connectors produce events
interpreters understand events
policies govern outcomes
```

---

## 2. Event

Every source emits the same normalized event shape.

Example:

```json
{
  "id": "evt_01JXYZ",
  "type": "github.issue.opened",
  "source": "github",
  "timestamp": "2026-08-12T19:15:03-05:00",
  "subject": "heath0xff/mrmr#42",
  "data": {
    "title": "Add Telegram notifications",
    "body": "...",
    "author": "example-user"
  },
  "metadata": {
    "source_event_id": "123456"
  }
}
```

The runtime should make as few assumptions as possible about `data`.

The important fields are:

```text
id
type
source
timestamp
subject
data
metadata
```

Delivery is at-least-once in practice: webhooks retry, pollers can overlap.
`metadata.source_event_id` is therefore a dedup key: events are unique on
`(source, source_event_id)` (hash the payload when a source provides no id).
Duplicates are recorded and dropped, not silently discarded — they appear in
traces as `duplicate`.

Time discipline: all internal timestamps are UTC; `timestamp` carries the
source's original offset if it has one. Events persist both event time and
processing time — windowed filters and staleness rules depend on the
distinction.

---

## 3. Filter

Filters are deterministic checks that happen before invoking a model.

Examples:

```yaml
filter:
  - field: data.author
    op: neq
    value: dependabot[bot]
```

Or:

```yaml
filter:
  script: |
    return event.data.size_bytes < 500000
```

The goal is simple:

> Do not spend inference on things normal software can already decide.

Windowed conditions for sensor-like sources:

Point-in-time checks are not enough when a source emits continuously — a
people detector fires 30 boxes/second, a motion sensor emits state changes,
not arrivals. Two rules keep this sane:

- **Collapse in the source adapter.** Debounce, dwell windows, per-subject
  cooldowns, and confidence thresholds run before an Event is persisted.
  Most raw readings must never become Events at all; SQLite and the trace
  log depend on this.
- **Windowed filters in flows** for cross-event conditions that survive
  collapsing:

  ```yaml
  filter:
    - window: 60s
      min_count: 3
      match:
        type: camera.*.person_detected
    - since_last:
        type: motion.any
        longer_than: 10m
  ```

  Window state lives in SQLite alongside source cursors and survives
  restarts.

Note also: detector confidence (box score in `event.data`) is not semantic
confidence (`decision.result.importance`). The first gates ingestion, the
second gates outcomes. Policy conditions must never conflate them.

---

## 4. Interpreter

An **Interpreter** uses a model to convert an event into structured meaning.

### Context assembly

Prompts rarely stand alone — several flows need recent history ("device +
recent events"). Context assembly is an explicit step between Filter and the
model call: the flow declares what history it needs, the runtime queries
persisted events and renders them into the prompt template.

```yaml
interpret:
  model: fast-local
  context:
    - last: 10
      type: homelab.device.*
      subject: "{{ event.subject }}"
    - since: 24h
      types: [github.issue.opened]
  prompt: |
    Recent related events:
    {{ context }}
    ...
```

It stays deterministic (queries, not model calls) and everything included is
visible in the trace.

Example input:

```text
A new GitHub issue was opened.

Determine:
- category
- importance
- whether action is required
- confidence
```

Example output:

```json
{
  "category": "bug",
  "importance": 0.87,
  "requires_action": true,
  "confidence": 0.94,
  "summary": "User reports a reproducible crash when the Discord adapter reconnects."
}
```

The interpreter is the semantic layer.

It should support structured output schemas so downstream logic does not depend on parsing prose.

Example configuration:

```yaml
interpret:
  model: fast-local
  schema:
    category:
      type: string
      enum: [bug, feature, support, spam]
    importance:
      type: number
      minimum: 0
      maximum: 1
    requires_action:
      type: boolean
    confidence:
      type: number
      minimum: 0
      maximum: 1
    summary:
      type: string
```

Validation failure handling:

- Output that fails the schema is retried once, with the validation error
  appended to the prompt.
- Still invalid → record a Decision with `status: invalid`, route to ignore,
  mark in the trace. Never crash the flow, never loop.

Local small models will hit this path regularly; it must be boring.

Model unavailability:

- Bounded retries with backoff (e.g. 3 attempts), then record a Decision with
  `status: errored` and route to ignore. Never park events for reprocessing by
  default — a batch of stale interpretations firing later is worse than quiet
  drops.
- Fail toward `ignore`, never toward `act`. An outage degrades mrmr into a
  no-op, which is the safe direction.
- A persistent endpoint failure trips that connection's circuit breaker so
  other flows keep working.

---

## 5. Decision

The interpreter returns a **Decision**.

A normalized decision might look like:

```json
{
  "event_id": "evt_01JXYZ",
  "interpreter": "issue-classifier",
  "model": "local-fast",
  "result": {
    "category": "bug",
    "importance": 0.87,
    "requires_action": true,
    "confidence": 0.94
  },
  "latency_ms": 183
}
```

Decisions are persisted.

They are part of the audit trail and can themselves become future events.

Stored decisions carry their schema version; a flow's result schema will
evolve, and old decisions must stay readable instead of silently mismatching
new validation rules.

A Decision may be re-emitted into the runtime as a new Event (e.g.
`agent.task.completed` in the delegation phase). This is an ordinary
outcome adapter creating an Event — not a separate pipeline — which is what
makes recursive workflows visible instead of hidden.

Recursion needs a guard: every emitted Event carries a causal depth counter,
incremented per hop. At max depth (v0 default: 10), the emitted event is
recorded but forced to ignore, flagged in the trace. This covers emit-event
actions, decisions-as-events, and delegated-agent completions alike.

---

## 6. Policy

Policy is where mrmr becomes safe and predictable.

The LLM recommends.

The runtime decides.

> **LLM = judgment**
> **runtime = authority**

Example:

```yaml
policy:
  - if:
      result.category: bug
      result.confidence: "> 0.85"
    then:
      delegate: agent://claude/triage-bug
```

Another:

```yaml
policy:
  - if:
      result.importance: "> 0.80"
      result.requires_action: true
    then:
      notify:
        via: telegram
        target: personal
```

Condition semantics (v0.1):

- All conditions within one `if` are ANDed. There is no OR and no nesting;
  express alternatives as separate rules.
- A value is either a literal (equality) or an operator string:
  `"> x"`, ">= x"`, `"< x"`, `"<= x"`, `"!= x"`.
- Rules evaluate top to bottom; the first match wins. No match falls through
  to `default`.

Deliberately minimal. If real flows demand OR or regex, add operators later —
the `field: value` rule shape does not change.

Policy should be explicit, deterministic, inspectable, and versionable.

An agent must never be able to grant itself new capabilities.

---

# Routing and fan-out

When an Event arrives, every Flow whose `on:` trigger matches its type and
connection processes it — independently, each with its own trace. There is
no first-match-wins across flows; that would make behavior depend on file
load order. Within a flow, policy rules are first-match-wins as described
above. Per-flow worker isolation (see Runtime storage) keeps one heavy flow
from starving others.

---

# Outcomes

mrmr has four first-class outcomes.

Outcome execution is at-least-once: the runtime persists the execution intent
*before* performing the side effect and records the result after. A crash in
between may repeat a notification on recovery — accepted, and adapter authors
should prefer idempotent effects where cheap.

## Ignore

Do nothing.

This is not a failure state.

Most events should probably end here.

```text
new event
↓
interpreted as irrelevant
↓
ignore
```

The ability to confidently ignore noise is one of the primary reasons to run ambient AI at all.

---

## Notify

Surface information to a human.

Notification adapters should include:

```text
Slack
Discord
Telegram
email
ntfy
Gotify
desktop notification
generic webhook
```

Example:

```yaml
then:
  notify:
    via: discord
    target: homelab
    message: "{{ decision.summary }}"
```

Notification is a first-class action because a large percentage of useful ambient AI is:

> “I watched this so you did not have to. Now something happened that deserves your attention.”

---

## Act

Perform a bounded deterministic action.

Examples:

```text
HTTP request
CLI command
shell script
GitHub API call
Jira API call
calendar update
email label
publish MQTT message
MCP tool call
emit another mrmr event
```

Example:

```yaml
then:
  action:
    type: http
    method: POST
    url: http://service.local/restart
```

Actions should have explicit permission boundaries.

---

## Delegate

Hand the event and context to a more capable agent.

Possible backends:

```text
Claude Agent SDK
Codex
Hermes
Pi
OMP
local agent harness
generic HTTP agent
generic CLI agent
```

Example:

```yaml
then:
  delegate:
    agent: research
    prompt: |
      Investigate this event and determine whether I need to take action.
```

The runtime should not try to become every agent framework.

Its job is to decide **when an agent deserves to wake up**.

---

## Shadow

A variant of `ignore` for going to production safely: the full pipeline runs,
everything is persisted and traced, but no effect leaves the runtime. The
outcome is recorded as if it had fired.

```yaml
default:
  shadow: true
```

Shadow mode is how a new flow earns trust: run it for a week against real
events, compare recorded outcomes against what you would have done manually,
tune filters/policy thresholds, then flip it live. Because decisions and
traces persist identically, promotion is a config change, not a rewrite.

---

# Model architecture

mrmr should be completely model-agnostic.

Example:

```yaml
models:
  fast-local:
    provider: openai-compatible
    base_url: http://deepthought:8000/v1
    model: local-small-model

  gemma:
    provider: openai-compatible
    base_url: http://marvin:8000/v1
    model: gemma

  frontier:
    provider: anthropic
    model: claude-sonnet
```

Initial model providers:

```text
OpenAI-compatible
Anthropic
Ollama
llama.cpp
vLLM
OpenRouter
custom HTTP
```

The preferred architecture is:

```text
event
  ↓
deterministic filters
  ↓
cheap local model
  ↓
interesting?
  ├─ no  → ignore
  └─ yes
      ↓
  bounded action possible?
      ├─ yes → act / notify
      └─ no
          ↓
       delegate
          ↓
     heavyweight agent
```

Local models should handle the overwhelming majority of interpretation.

Large cloud or local frontier models should be escalation paths.

Structured output must be enforced by the inference stack, not hoped for:
prefer OpenAI-compatible endpoints with schema-constrained decoding
(llama.cpp grammars, vLLM guided JSON, Ollama structured outputs). This moves
schema-valid rates from ~85% to ~100% on simple schemas and makes the retry
path in §4 a rare fallback instead of a routine event.

---

# Agent backends

Agents should use a simple adapter interface.

Conceptually:

```text
AgentRunner
├── ClaudeAgentSDK
├── Codex
├── Hermes
├── Pi
├── OMP
├── HTTP
└── Exec
```

Example configuration:

```yaml
agents:
  research:
    type: http
    endpoint: http://hermes:8080/tasks

  coding:
    type: exec
    command: codex

  triage:
    type: claude-agent-sdk
```

The generic HTTP and executable adapters are important.

They prevent mrmr from requiring native integrations for every agent framework.

---

# MCP

MCP is supported, but it is not foundational infrastructure.

mrmr should treat MCP as another way to invoke tools.

```text
event
  ↓
interpret
  ↓
policy
  ↓
agent or action
  ↓
CLI / API / MCP / HTTP
```

MCP does not need to be how events enter the runtime.

Events should arrive through mechanisms appropriate to the source:

```text
webhooks
polling
MQTT
filesystem events
message streams
cron
local IPC
```

Exception: MCP as a *polled source*.

Many SaaS products expose MCP servers for their data (email, calendars,
tasks) without offering webhooks or friendly APIs. mrmr should support an
MCP poller source: on a schedule, call a configured MCP tool/resource,
normalize the result set into events (one per new record, keyed by cursor),
and feed them through the normal pipeline.

```yaml
sources:
  - type: mcp-poller
    connection: superhuman
    tool: list_recent_messages
    every: 5m
    cursor_key: id
    event_type: email.received
```

This stays an adapter, not core: the runtime sees ordinary polled events and
does not know MCP exists.

---

# Event ingestion

Sources should support three main ingestion patterns.

## Webhooks

Preferred when available.

```text
GitHub
Jira
Stripe
custom service
    ↓
POST /api/events/webhook/...
    ↓
mrmr
```

---

## Polling

Perfectly acceptable when push does not exist.

Example:

```text
every 60 seconds
  ↓
fetch events after cursor
  ↓
normalize new records
  ↓
update cursor
```

Poll state should be persisted so restarts are safe.

Two built-in pull adapters cover most of it: a generic HTTP poller
(cursor-based REST/JSON) and the MCP poller described in the MCP section
(SaaS products that only expose MCP). Anything more specific is a recipe,
not a source implementation.

---

## Streams / watchers

Used for local and infrastructure events.

Examples:

```text
MQTT
filesystem watcher
log stream
systemd journal
Tailscale status
Redis pub/sub
NATS
```

---

# Runtime storage

v0.1 should use **SQLite**.

Do not begin with distributed infrastructure.

SQLite can hold:

```text
events
decisions
actions
delegations
notifications
source cursors
retries
flow definitions
audit logs
credentials metadata
```

Example conceptual tables:

```text
events
decisions
executions
flows
sources
connections
agents
```

A later version can support Redis Streams, NATS JetStream, or another durable event backend.

Do not require them initially.

Operational notes:

- The events table doubles as the queue. Workers poll for unclaimed events
  with bounded concurrency; ingestion never blocks on interpretation, and
  SQLite WAL mode handles the reader/writer mix.
- Stale interpretations must not act: `act`/`delegate` outcomes check event
  age against a per-flow `max_age` (default e.g. 15m). Expired → downgrade to
  notify (or shadow) instead of acting on outdated judgment.
- Flows run on isolated workers so a wedged model endpoint on one flow cannot
  starve the rest.

---

# Safety and permissions

Permissions are part of the runtime, not prompts.

Example:

```yaml
permissions:
  github.read: allow
  github.comment: allow
  github.close_issue: approve

  email.read: allow
  email.send: approve

  shell.read: allow
  shell.write: deny

  calendar.read: allow
  calendar.write: approve
```

Possible permission levels:

```text
allow
approve
deny
```

`approve` means the runtime creates an approval request rather than executing automatically.

Eventually:

```text
allow_once
allow_for_flow
allow_for_source
allow_until
```

Every action should record:

```text
which event caused it
which decision recommended it
which policy allowed it
which permissions were evaluated
which arguments were passed
what happened
```

Secrets live outside flow definitions and outside traceable state: values are
stored in environment variables (or OS keychain) and referenced by name from
connections (`api_key_env: SUPERHUMAN_API_KEY`). SQLite holds only metadata —
configured/missing status, last-checked timestamp. This is why the UI can
display health without ever touching raw values.

Approval requests expire. A pending `approve` older than its TTL (default
e.g. 1h) auto-denies with a trace entry — an approval for "restart server"
must not stay valid until someone cleans the queue next week.

---

# Local web UI

mrmr should ship with a local web interface.

The first version should avoid becoming an enormous visual-programming IDE.

Start with four areas:

```text
Flows
Events
Agents
Connections
```

---

## Flows

Flows define how events move through the runtime.

A simple editor might represent:

```text
WHEN

[ GitHub: issue opened ]

IF

[ ignore bots ]

INTERPRET

[ local model: classify issue ]

THEN

[ bug + confidence > .85 ]

→ delegate to Claude

ELSE

→ notify Discord
```

The UI can begin as form-based configuration rather than a node canvas.

A visual graph can come later.

---

## Events

A chronological event stream:

```text
19:42 github.issue.opened
19:42 filter passed
19:42 local model → bug, confidence .94
19:42 policy matched → triage-bugs
19:42 delegated → claude
19:43 github.label.added → bug
✓ complete
```

Users should be able to click an event and inspect the entire trace.

---

## Agents

Configure delegation targets:

```text
Claude Agent SDK
Codex
Hermes
Pi
OMP
HTTP
Exec
```

Each agent displays:

```text
status
endpoint
model
capabilities
permissions
recent executions
```

---

## Connections

Configure integrations:

```text
GitHub
Gmail
Google Calendar
Discord
Slack
Telegram
MQTT
generic webhook
OpenAI-compatible model endpoint
```

Secrets should never appear in flow definitions.

Flows refer to connection IDs.

---

# Observability

Observability is not optional.

When autonomous software behaves unexpectedly, the first question is:

> Why did this happen?

Every execution should have a trace:

```text
event
→ source
→ filter results
→ interpreter (model name, latency, schema-valid?)
→ structured decision
→ policy match
→ permission evaluations
→ outcome + adapter + result/error summary

Full prompts and raw model responses are not persisted by default.
Opt-in debug capture only, never in normal traces.
```

A trace should make it possible to answer:

```text
What event caused this?
What model saw it?
What context did the model receive?
What did the model decide?
Which policy matched?
Why was this action permitted?
What command/API call was executed?
What was the result?
```

Long term, mrmr should expose OpenTelemetry-compatible traces and metrics.

---

# Flow configuration

Example:

```yaml
name: github-issue-triage

on:
  github.issue.opened:
    connection: personal-github

filter:
  - field: event.data.author
    op: neq
    value: dependabot[bot]

interpret:
  model: fast-local
  prompt: |
    Classify this GitHub issue.

    Determine:
    - category
    - importance
    - whether it requires action
    - confidence
    - one-sentence summary

  schema:
    category:
      type: string
      enum: [bug, feature, support, spam]
    importance:
      type: number
      minimum: 0
      maximum: 1
    requires_action:
      type: boolean
    confidence:
      type: number
      minimum: 0
      maximum: 1
    summary:
      type: string

policy:
  - if:
      result.category: spam
      result.confidence: "> 0.90"
    then:
      action:
        type: github.close_issue

  - if:
      result.category: bug
      result.confidence: "> 0.85"
    then:
      delegate:
        agent: claude-triage

  - if:
      result.importance: "> 0.80"
    then:
      notify:
        via: telegram
        target: personal
        message: "{{ result.summary }}"

default:
  ignore: true
```

Reload semantics: flows carry a content hash/version. In-flight executions
finish under the definition they started with; new events use the current
one. Invalid config never replaces a running flow — validate first, keep the
last known-good set active.

---

# Example flows

## Inbox attention

```text
Gmail new message
    ↓
ignore newsletters / automated senders
    ↓
local model
    ↓
requires attention?
    ├─ no → ignore
    ├─ FYI → store
    ├─ response needed → notify
    └─ work required → delegate
```

---

## Calendar preparation

```text
calendar event created or changed
    ↓
local model
    ↓
does this meeting require preparation?
    ├─ no → ignore
    └─ yes
        ↓
research agent
        ↓
meeting brief
        ↓
Telegram notification
```

---

## GitHub issue triage

```text
issue opened
    ↓
local model
    ↓
classify + identify likely owner
    ↓
confidence > threshold?
    ├─ yes → label / assign
    └─ no → delegate investigation
```

---

## Homelab watchdog

```text
device offline
    ↓
local model receives device + recent events
    ↓
expected?
    ├─ yes → ignore
    └─ no
        ↓
diagnostic agent
        ↓
notify Discord
```

---

## RSS research

```text
RSS item published
    ↓
keyword filter
    ↓
local relevance model
    ↓
relevant?
    ├─ no → discard
    └─ yes
        ↓
research agent
        ↓
summary / save / notify
```

---

## MQTT anomaly interpretation

```text
MQTT event
    ↓
heuristic threshold
    ↓
local model gets recent sensor context
    ↓
normal / suspicious / actionable
    ↓
ignore / notify / act
```

---

# Recipes

mrmr should ship examples as **recipes**, not built-in agents.

```text
recipes/
├── github-issue-triage/
├── inbox-attention/
├── calendar-prep/
├── homelab-watchdog/
├── rss-research/
├── mqtt-anomaly/
└── pr-review-router/
```

A recipe contains:

```text
flow definition
required connections
required permissions
sample model schema
README
```

Users supply their own models, infrastructure, and credentials.

---

# Suggested repository structure

A Go daemon is a strong fit for the runtime because it can compile to one binary, handles concurrency well, and is straightforward to self-host.

The web UI can be a separate TypeScript app embedded into the binary for release builds.

```text
mrmr/
├── cmd/mrmr/
├── internal/
│   ├── runtime/
│   ├── events/
│   ├── flows/
│   ├── policy/
│   ├── storage/
│   └── tracing/
├── web/
├── migrations/
├── mrmr.example.yaml
├── Makefile
└── README.md

Sources, model clients, notification/action/agent adapters live under
`internal/` until a second real implementation justifies promotion to their
own package. No placeholder directories.
```

---

# CLI

The CLI should be usable without the web UI.

Possible commands:

```bash
mrmr init
mrmr run
mrmr eval --dataset ./testdata/generated-eval.jsonl
mrmr status

mrmr events
mrmr events tail
mrmr inspect evt_01JXYZ

mrmr flows
mrmr flow validate ./flows/github.yaml
mrmr flow run github-issue-triage --fixture issue.json

mrmr sources
mrmr agents
mrmr connections

mrmr approvals
mrmr approve act_123
mrmr deny act_123
```

Developer experience matters.

A new user should be able to reach a working flow quickly.

---

# Deployment

Initial deployment targets:

```text
single binary
Docker
Docker Compose
```

Example:

```bash
docker compose up -d
```

Then:

```text
http://localhost:4242
```

mrmr should not initially require:

```text
Kubernetes
Kafka
Redis
Postgres
NATS
a cloud account
```

The default experience should work on:

```text
a laptop
a Raspberry Pi
a mini PC
a homelab server
a DGX Spark
```

---

# v0.1

v0.1 is Phase 0 plus durability — nothing more:

- POST /api/events (normalized event, persisted)
- OpenAI-compatible interpreter with schema-constrained decoding and
  JSON-schema validation
- deterministic filters
- policy evaluation (semantics above)
- all four outcomes plus shadow, wired to: stdout notify, HTTP action,
  emit-event, generic-HTTP delegate
- SQLite storage (events, decisions, executions)
- trace printed per event / queryable via CLI
- golden-set gate: ≥50 labeled real events run through the interpreter with
  measured accuracy and false-ignore rate before any flow is called done

Explicitly moved out of v0.1 into their phases: Discord/Telegram adapters,
cron + poller sources, exec actions, permissions/approval queue, web UI,
flow editor. The phased list above is authoritative; this section no longer
duplicates it with a broader scope.

---

# Explicitly not v0.1

Do not build these yet:

```text
distributed runtime
multi-user authentication
cloud hosting
marketplace
billing
mobile app
complex node canvas
vector database
general-purpose memory framework
custom agent framework
custom message broker
Kubernetes operator
plugin marketplace
```

The project should remain understandable.

---

# Development phases

## Phase 0 — prove the loop

Build:

```text
POST /events
    ↓
persist Event
    ↓
call OpenAI-compatible model
    ↓
produce structured Decision
    ↓
evaluate policy
    ↓
print outcome
```

Nothing else matters until this loop works.

Success criteria:

```text
curl event into mrmr
→ local model interprets it
→ deterministic policy routes it
→ trace shows exactly why
```

---

## Phase 1 — useful daemon

Add:

```text
SQLite
flow YAML
cron source
HTTP poller
Discord
Telegram
HTTP actions
exec actions
retries
permissions
```

At this point mrmr is useful without the UI.

---

## Phase 2 — local UI

Add:

```text
event timeline
trace inspection
flow configuration
connections
agents
approvals
runtime health
```

Focus on visibility rather than visual programming.

---

## Phase 3 — real integrations

Add integrations based on actual dogfooding:

```text
GitHub
Gmail
Google Calendar
MQTT
Slack
Jira
filesystem
Tailscale
mcp-poller source (Superhuman-style SaaS via MCP)
```

Do not implement integrations just because they exist.

Build them when a real recipe needs them.

---

## Phase 4 — delegation

Add first-class adapters for:

```text
Claude Agent SDK
Codex
Hermes
Pi / OMP
```

Delegated agent completion should emit another event back into mrmr.

Example:

```text
agent.task.completed
```

This enables recursive workflows without hiding execution from the runtime.

---

## Phase 5 — ecosystem

Once the abstractions are stable:

```text
source SDK
action SDK
notification SDK
agent adapter SDK
recipe repository
community integrations
optional NATS backend
OpenTelemetry
```

---

# First implementation

Do not begin with GitHub, Gmail, Hermes, or a graphical editor.

Build the smallest possible vertical slice.

## Milestone 1

Create:

```text
mrmr run
```

Expose:

```text
POST /api/events
```

Accept:

```json
{
  "type": "test.message",
  "source": "curl",
  "data": {
    "message": "The production API has returned 500 errors for five minutes."
  }
}
```

Send the event to a configured OpenAI-compatible local model.

Require:

```json
{
  "importance": 0.92,
  "requires_action": true,
  "category": "incident",
  "summary": "Production API is repeatedly failing."
}
```

Evaluate:

```yaml
policy:
  - if:
      result.importance: "> 0.8"
    then:
      notify:
        via: stdout
```

Print the trace.

That is mrmr.

Done when: the loop runs end-to-end, and a labeled set of ≥50 real events
interpreted with measured accuracy and false-ignore rate passes your bar.
A loop that runs but hasn't been graded is not done.

Everything else is an adapter around that loop.

---

# First dogfood flows

After the core works, build these in order.

## 1. Generic webhook → local interpretation → Discord

This proves the complete architecture with minimal external complexity.

## 2. RSS → relevance → Telegram

This proves polling and high-volume ignore behavior.

## 3. GitHub issue → classify → action / delegate

This proves richer policies and real-world actions.

## 4. Homelab event → interpret → notify

This proves local ambient infrastructure.

## 5. Email → attention classification

This is likely the first flow where mrmr begins to feel genuinely ambient.

---

# Design principles

## Local-first

A user should be able to run the entire decision loop without sending events to a cloud model.

Cloud models are optional escalation paths.

---

## Boring infrastructure

Prefer:

```text
SQLite
HTTP
YAML
JSON
single process
single binary
```

over unnecessary distributed systems.

---

## AI only where ambiguity exists

Do not use an LLM to do what an `if` statement can do.

---

## Structured decisions

Models should return schemas, not prose that another component has to reinterpret.

---

## Explicit authority

Models recommend.

Policies authorize.

---

## Everything is inspectable

No invisible agent magic.

Every decision must have a trace.

---

## Framework agnostic

mrmr is not:

```text
a Claude framework
a Codex framework
a Hermes framework
an MCP framework
an Ollama framework
```

It sits underneath them.

---

## Events are the primitive

The runtime is driven by things that happened, not conversations.

---

## Agents are escalation

Agents should be invoked because an interpreted event requires open-ended work.

They should not be the default execution primitive.

---

## Ignore is valuable

The runtime creates value by suppressing irrelevant events as much as by acting on important ones.

---

# Non-goals

mrmr is not intended to become:

- a hosted SaaS product
- a general chatbot
- a replacement for n8n
- a replacement for agent harnesses
- an AI operating system
- a universal personal assistant
- a general-purpose workflow engine
- a new LLM inference server
- a new MCP implementation

It is a small, composable layer between **events** and **intelligent outcomes**.

---

# Positioning

A useful shorthand is:

> **n8n-style event automation with an AI interpretation layer and strict policy boundaries.**

But the deeper distinction is:

Traditional automation:

```text
event → deterministic workflow
```

mrmr:

```text
event
  ↓
semantic interpretation
  ↓
structured decision
  ↓
deterministic authority
  ↓
ignore / notify / act / delegate
```

---

# Naming

**mrmr**

Pronounced:

> murmur

Possible tagline:

> **Let your systems murmur. Wake the agents only when it matters.**

Alternative:

> **Ambient AI, without the ambient chaos.**

Technical description:

> **mrmr is a local-first event runtime that uses AI to decide what deserves attention, action, or an agent.**

---

# Open-source philosophy

The project should be:

```text
local-first
self-hosted
model-agnostic
agent-agnostic
hackable
observable
permissioned
boring where possible
```

Users should bring:

```text
their own models
their own agents
their own infrastructure
their own credentials
their own policies
```

mrmr provides the substrate that connects them.

---

# The invariant

If the project becomes complicated, come back to this:

```text
                 ┌→ ignore
                 ├→ notify
event → interpret├→ act
                 └→ delegate
```

If a feature does not make that loop more useful, safer, easier to inspect, or easier to extend, it probably does not belong in the core.
