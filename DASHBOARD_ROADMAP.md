# Dashboard roadmap

Status: delivery plan with an implemented management slice (see below). Product direction confirmed with Heath:
**make the dashboard the main operating interface; improve the current system
before expanding it.** This document does not authorize implementation or live
policy changes. Baseline: `000c194`.

[Management guide](README.md#configuration-and-management-ui) ·
[Read-only dashboard](README.md#read-only-dashboard) ·
[Setup runbook](SETUP.md) · [Runtime design](IMPLEMENTATION.md#local-web-ui)

## Current implementation status

The management requirement was prioritized ahead of completing every diagnosis
and evaluation milestone. An optional authenticated `/manage/` listener now
provides Overview, Activity, Review, Configure, and Runtime in one binary:

- Revision-checked drafts, shared config validation, model/source/basic-policy
  forms, and a full YAML editor.
- Shadow-only activation, retained source cursors, source pause/resume through
  configuration, prior-revision recovery, and a persistent return-to-shadow gate.
- Protected first-run setup with collection disabled until activation.
- Visible Save configuration and Review and apply controls, with separate draft
  and active states. Discord webhook enrollment uses authenticated writes to a
  private credential store; values never enter configuration history or responses.
- Current configuration lists active monitors separately from drafts. Add/edit
  system-service workflows reuse model connections and select source-local
  questions/policy plus Discord or stdout notifications. A bounded two-example
  test selects the specific source. Activation permits explicitly confirmed live
  notifications, while actions/delegation remain shadow-only. Journal scope can
  select system services or the runtime account's user services. Advanced
  retains other source types, shared defaults, and custom contracts.
- Browser labels, explicit origin and uncertainty, CLI conflict checks, label
  history, and an operator audit.
- Bounded saved policy comparisons and interpreter-and-policy evaluation using
  the CLI evaluator. Recent real reviewed events supply a frozen label snapshot;
  policy comparisons require historical interpretation contracts.
- Stable receive-time event pages, parent/child links, and phone navigation.

This does **not** mark D1–D5 complete. Remaining work includes reusable dataset
selection and coverage tools, broader evaluation analysis, guided interpretation,
filter and agent forms, source previews, authorized delivery tests, and evidence-gated
live promotion. Operator tokens require external provisioning. Network listener
and database changes still require restart. Read-only access has not gained write
permission. The management implementation and its tests are described in README.

| Milestone | Current delivery | Remaining scope |
| --- | --- | --- |
| D0 | Read-only foundation shipped | Preserve its access and API contracts |
| D1 | Separate task pages, fixed receive-time Activity pages, related Events, Back navigation, and phone navigation | Complete the broader diagnosis links and coverage views |
| D2 | Browser labels, uncertainty/origin, correction history, CLI-aware conflicts | Sampling queues and reusable, coverage-checked datasets |
| D3 | Saved, cancellable policy and interpreter evaluations over up to 50 recent reviewed real Events | Reusable dataset comparisons and broader regression analysis |
| D4 | Draft validation/differences, shadow activation, source draining, revision recovery, durable safety stop | Full effect-free pipeline checks and evidence-gated live promotion |
| D5 | Protected first-run setup, current monitor list, source-local service setup, reusable model/Discord references, bounded synthetic model test, source pause/resume | Source previews, delivery tests, advanced forms, and broader connection lifecycle |
| D6 | Not implemented | Independent Flows, enforced approvals, and adapter-specific retry/cancel controls |

Verification includes offline Go/race tests, a 10,000-event storage fixture,
shutdown with an in-flight HTTP event, activation failures, migration rollback,
and optional synthetic Chrome journeys at 390px and 1440px. Browser checks cover
forms, activation/recovery, review, hostile text, exact numeric IDs, and locking
while a response body is pending. These checks do not establish real-event model
quality, remote deployment health, or cross-browser/accessibility certification.

## 1. Purpose and order

The dashboard should answer five questions:

1. Is mrmr collecting and processing the events I expect?
2. What happened, and why did it select this outcome?
3. Was that decision useful and correct?
4. What change would improve it, and what evidence supports that change?
5. How do I apply that change without granting unintended authority?

The main loop is:

```text
Observe → Investigate → Review → Compare → Change safely → Observe
```

Expansion uses that same loop, not a separate setup experience:

```text
Choose a purpose → Connect → Select scope → Preview → Start in shadow
    → Collect real events → Review → Evaluate → Consider live effects
```

The first user is a single operator managing one runtime. Desktop supports
investigation, comparison, and editing. Phone use supports quick checks, event
inspection, and individual reviews. The CLI remains available for recovery and
automation; the dashboard must not require routine SSH work to complete its
supported tasks.

**Success is a closed improvement loop, not more charts.** A user can identify a
bad outcome, label what should have happened, test a correction, and verify the
active revision without losing the original evidence.

## 2. Original baseline and planned capabilities

This table records the starting point at `000c194`. The implementation status
above takes precedence for the current code; the remaining sections retain the
full target behavior and exit tests.

| Area | Available at the original baseline | Planned work |
|---|---|---|
| Visibility | Embedded dashboard; private HTTPS; runtime response and process-local poll health | Task-focused overview, deep links, stronger diagnosis |
| Event inspection | Search, filters, Decisions, Executions, traces; parent/child data in inspection API | Connected event journeys and clearer explanations |
| Human review | CLI labels and dataset export; one current label per Event | Protected browser writes, label history, conflict handling, review workflow |
| Evaluation | CLI interpreter-and-policy evaluation; accuracy, false ignores, per-class results | Comparable saved runs, dataset versions, browser workflow |
| Configuration | Strict YAML validation at startup; one shared pipeline and event-type interpreter overrides | Drafts, version history, safe activation, rollback |
| Integrations | HTTP polling, system journals, model endpoints, HTTP agents, stdout/Discord; standalone Superhuman OAuth/check | Guided setup and lifecycle controls; new adapters only for actual needs |
| Permissions | Deterministic policy and shadow gates | Operator write access; runtime approval/permission system before effect approvals |

Original baseline limits (not current implementation status):

- The current dashboard listener is read-only. Tailnet access is not permission
  to change labels, configuration, or effects.
- An empty successful poll is healthy. An old model success is historical
  evidence, not a current health probe. Poll history resets on restart.
- An Execution records intent and its observed result. Pending intent does not
  prove that an effect did or did not happen. There is no automatic replay.
- CLI evaluation invokes the interpreter and evaluates policy, without effects
  or runtime Event writes. It does **not** run deduplication, pre-model filters,
  or the depth guard. It does not test worker execution or delivery.
- Current labels replace prior labels. They are not yet an audit history.
- The runtime does not yet have independent Flows, hot reload, a general
  connection manager, or an approval queue. Do not draw controls that imply it does.

## 3. Information architecture

The management UI now has these four primary areas and a Runtime utility page.
The table describes their full target scope; the delivery table above records
what is implemented.

| Area | Main question | Contents and next step |
|---|---|---|
| **Overview** | What needs attention now? | Collection health, failures, unfinished work, review backlog, effect boundaries; each item opens the relevant evidence |
| **Activity** | What happened? | Searchable Events and event detail, Decisions, Executions, causal links, recorded configuration references |
| **Review** | Was it correct, and are we improving? | Label queue, reviewed Events, dataset coverage, evaluation runs and comparisons; effect approvals are a separate future task |
| **Configure** | What is allowed to happen? | Current pipeline, Sources, Interpreters/models, Policy, connection references, Agents, drafts, tests, revisions, and activation |

Keep runtime/access information in a small **Runtime** utility page. It is not
an operating-system admin panel. Do not add a top-level navigation item for
every database table or integration.

The shell should identify the runtime host, operator access, and the active
configuration revision. Show effect mode as
**shadow**, **live**, or **mixed** from actual rules; never infer it from the last
Event. Historical detail shows the mode recorded for that execution.

### Overview is a starting point, not a second event table

Order its content by the next useful action:

1. Runtime freshness and source failures or overdue polls.
2. Failed/invalid Decisions, adapter errors, and unfinished Executions.
3. Events awaiting human review and gaps in evaluation coverage.
4. Collection activity, model latency, and effect counts for a stated window.

These are deterministic summaries, not AI incident diagnoses. Separate
"needs investigation" from "confirmed incident." Counts link to the exact
filtered list that produced them. Do not hide uncertainty behind a single
health score or mark silence as success.

### Activity owns the event detail

Use one canonical event URL, whether the user arrives from Overview, Review,
a model comparison, a notification, or a parent Event. Preserve filters, time
range, page position, and return destination in navigation.

Read detail in pipeline order:

```text
Source → Event → Filter → Interpreter → Decision → Policy → Outcome → Adapter
```

Lead with a plain result: "Notification recorded in shadow," "Ignored after a
model timeout," or "Execution result not recorded." Then show the supporting
records. Keep raw JSON available, but not as the main explanation.

Display structured model answers, not an invented explanation or hidden
chain of thought. A missing record stays missing. If the historical policy is
not retained, show its recorded reference/hash and say the definition is
unavailable; do not explain an old outcome using today's rules.

Parent/child links connect delegation and completion events. Agent acceptance,
terminal completion, and unknown result are distinct states. Do not invent
progress, cancellation, or asynchronous status from a synchronous HTTP response.

### Review is work, not another feed

The operator selects a queue, inspects an Event, records the expected judgment
and outcome, then saves and moves to the next item. Support defer/uncertain
without forcing a false label. Separate uncertainty from the existing boolean
`requires_action` field.

Offer chronological and deterministic sampling views across sources, event
types, failures, ignored Events, and repeated-message groups. Reviewing only
notifications would hide false ignores. Group repetition for navigation, but
do not label every repeated message from one example or assume it is harmless.

Do not prefill the human answer from the model answer. Make the model result
available for comparison without making agreement the easiest default.
"Save label" changes review data only; it never reruns an Event, trains a model,
sends a notification, or approves an effect.

### Configure is a guided pipeline, not a node canvas

Show **Current configuration** with active source-local monitors and a separate
**Add monitoring service** workflow, not a list of fictional independent Flows:

```text
Source → shared deterministic filters → source-local or legacy interpretation
       → selected ordered policy → its default outcome → allowed adapter
```

Use forms backed by the same runtime validator, with YAML available for expert
inspection/export. Make first-match rules, shared defaults, source-local policy,
and event-type interpreter overrides visible. Keep model roles distinct: a decision model is
not an agent harness, and a worker model does not grant tool authority.

Only introduce independent Flow creation after the runtime implements Flow
identity, routing, execution attribution, and isolation. Preserve this task
structure when that capability arrives.

## 4. Delivery milestones

These labels retain the original delivery plan; they do not describe a strict
implementation sequence or renumber the phases in `IMPLEMENTATION.md`. The
management slice delivered parts of D4 and D5 before the complete review and
evaluation workflows. The requirements below remain target behavior, not claims
that every control or exit test is complete. Future live authority still depends
on the stated safety gates.

### D0 — Read-only foundation: shipped

Private access, source-poll evidence, searchable Events, inspection, shadow
boundaries, and historical latency are available. Preserve these APIs and the
read-only access boundary throughout the roadmap.

### D1 — Diagnose the current system

**Journey:** Overview → filtered Activity → event detail → related evidence.

- Separate Overview from Activity; keep links and useful context between them.
- Add stable event URLs, URL-backed filters/time range, and browser Back support.
- Explain filtered, ignored, failed, shadowed, delivered/accepted, and unfinished
  states from recorded evidence. Link parent and child Events.
- Link source/model/rule summaries to affected Events. Show freshness, data
  window, and missing evidence beside the value, not only in help text.
- Present repeated-message groups without suppressing the underlying Events.

**Runtime work:** bounded read queries and projections where needed. Do not add
model calls, historical poll charts without stored history, or write endpoints.

**Exit test:** using seeded examples, an operator can distinguish a quiet
collector from a failed collector, explain an ignored Event, and follow a
completion to its parent without SSH. Deep links and Back work on desktop and
phone; stale data is never presented as current.

### D2 — Review and label real Events

**Journey:** review queue → evidence → human judgment → save → next Event.

- Add category, expected action requirement, and expected outcome using the
  existing supported label contract and exact event-type interpreter selection.
- Show unsupported contracts explicitly; do not create guessed labels for them.
- Add defer/uncertain review state, label corrections, revision history, and
  conflict detection. A stale browser must not overwrite a newer CLI label.
- Show labeled real, synthetic, and unclassified records separately. Add explicit
  origin/curation metadata; do not infer "real" from message text or an ID prefix.
- Build a versioned dataset selection with coverage by source, event type,
  category, and expected outcome. Export remains an explicit private-data action.

**Runtime work:** the operator-write boundary below, shared CLI/web validation,
additive label/review history, and immutable dataset membership/label revisions.
Existing records keep unknown origin/history where evidence is absent; migration
must not invent it. No configuration activation or effect controls in this milestone.

**Exit test:** review a representative batch entirely in the browser. Saved
labels survive restart, corrections are traceable, concurrent edits conflict
safely, and uncertain/test records do not silently count toward the real-data
gate. No review request calls a model or an adapter.

### D3 — Compare improvements before changing behavior

**Journey:** choose dataset → select baseline/candidate → inspect scope and call budget
→ run comparison → inspect regressions → keep or reject the candidate.

Support two clearly named operations:

1. **Policy comparison:** evaluate candidate rules against stored Decisions.
   No inference or effects. Reject comparisons when the answer contract is
   incompatible; this cannot prove a new model or prompt works.
2. **Interpreter + policy evaluation:** make bounded calls to an explicitly
   selected, configured model using frozen Events and labels. Evaluate intended
   outcomes only. State that ingestion gates and adapter execution are excluded.

Show per-Event differences alongside category/outcome accuracy, false-ignore
counts and rate, per-class support, errors, timeouts, and comparable latency.
Show denominator and sample size; no eligible examples means "not measured,"
not 0% risk. Only show probability calibration when the provider supplies actual
probabilities.

Keep candidate configuration separate from active configuration. Reuse CLI
semantics and factor shared code only where these two real callers need it.
Evaluation records must include dataset/label revision, candidate revision,
model identity, evaluation scope, and completion state. Do not store rendered
prompts or raw external responses just to make a report.

**Runtime work:** bounded, cancellable evaluation jobs with explicit ownership,
concurrency limits, and saved results outside the operational Event history.
A restart marks interrupted work; it does not silently rerun calls. Show which
endpoint receives Event content, expected call count, and limits before starting
a run. Do not invent token usage or monetary cost when the provider does not
report them.

**Exit test:** compare two candidates on the same dataset, reopen the report,
and inspect every new false ignore. Cancellation/failure leaves honest partial
results. Operational Events and effects are unchanged. A candidate with a faster
model but worse safety does not receive an automatic recommendation to go live.

### D4 — Configure and operate the current pipeline safely

**Journey:** inspect active revision → draft → validate → compare → review diff
→ apply in shadow → observe → explicitly consider promotion.

Deliver in two increments:

- **D4a: draft and test.** Edit supported current-pipeline fields, show validation
  errors at the field, compare candidates, and export/stage YAML. If a restart
  is required, say so; saving a file must not claim the runtime changed.
- **D4b: activation and recovery.** Add a bounded runtime activation mechanism
  for the supported changes, with an active revision, last-known-good revision,
  readiness checks, and rollback. Do not implement activation by accepting shell
  commands or arbitrary service names from the browser.

The activation slice does not yet provide a full pipeline check. Explicit live
notification consent does not establish model quality or authorize actions/agents.
Before using changed filters/routing to justify live promotion, add a separately
named, effect-free check for the applicable deterministic filters, depth guard,
interpreter selection, and policy. Include would-be filtered Events in the results rather
than dropping them from the denominator. Test normalization and deduplication at
the source boundary. Neither this check nor interpreter evaluation proves live
adapter delivery; shadow observation and authorized delivery tests are separate.

Validate the whole candidate, show affected sources/rules/adapters, and require
confirmation for authority or data-scope changes. Detect external YAML edits by
revision; do not silently replace a newer file. Keep YAML export and CLI use
compatible rather than introducing a second, conflicting configuration authority.

In-flight Events must finish under their original revision. New Events use the
new revision only after successful activation. Preserve source cursors across
supported changes; changed source identity or lost checkpoints need an explicit
plan, never silent backfill or a jump to the tail. Show restart-required fields
instead of pretending every setting can hot reload.

Promotion is per intended effect scope, not a large global "Go live" switch:

- Require at least 50 representative labeled real Events for the relevant flow,
  with measured accuracy and false-ignore rate and operator-set acceptance limits.
- Show class coverage, repeated-example concentration, and held-out results.
  Keep near-duplicate groups together when splitting tuning and validation sets.
- Thresholds are chosen before the final evaluation. Fifty similar warnings,
  no positive cases, or synthetic data alone do not establish readiness.
- Tie evidence to the tested configuration and model. Material changes make
  previous readiness evidence stale; label that state and evaluate again.
- Require an explicit operator decision. Evaluation success never flips a rule
  live by itself. Enable notifications before higher-impact actions/delegation
  when that is the actual use case; test delivery separately from decision quality.

Provide a narrowly scoped **Return to shadow** control before live promotion.
Unlike ordinary revision activation, this safety gate must be checked immediately
before adapter dispatch, including work still interpreting under an older
revision. Keep its original Decision/Policy evidence and record the suppression.
It cannot recall a notification or undo work already sent. Record the cutoff and
in-flight work. Rollback changes future behavior, not history, labels, or
already-performed effects.

**Exit test:** invalid drafts and failed activation preserve the running revision;
restart and concurrent edits cannot leave an ambiguous active configuration.
The operator can apply a tested shadow revision, verify it on new Events, and
return live rules to shadow with a durable audit record.

### D5 — Expand through guided setup

**Journey:** choose purpose → choose supported source → provide connection
reference → select data scope → preview → test → activate in shadow → Review.

Start with the adapters already available. Add a new integration only for a
specific dogfood task; do not ship a catalog of unavailable integrations.

Distinguish adding to a running instance from **first-run setup**. The implemented
`-setup` mode permits an absent configuration file, starts no sources, and rejects
ingestion until an operator activates valid configuration. It requires the
management listener and operator token. An existing invalid file still blocks
startup; setup does not silently replace it. No placeholder model or broadly
bound ingestion server is started.

- Show selected system units, HTTP item/cursor mapping, event type, model data
  destination, and intended effects before activation.
- Show a normalized preview without committing operational Events or advancing
  the live cursor. Separate synthetic fixtures from authorized real samples.
- Treat source reads, OAuth, model calls, and delivery tests as distinct consent
  steps. A connection check can contact an external service even if it writes
  nothing locally. A delivery test is a real effect and must be labeled as such.
- Manage Connections by reference, required scopes, configured/missing state,
  and last check. Never return stored secret values to the browser. Discord now
  has narrow, confirmed webhook enrollment into private per-account files with
  fixed-host validation and revision checks. Other credentials still use the
  existing approved environment/store; broader enrollment remains separate work.
- Carry new source scope into Activity and Review. New coverage must earn its
  own evidence; success on a journal dataset does not validate an email flow.
- Add pause/resume for supported sources with explicit cursor behavior. Pausing
  collection is different from returning effects to shadow; show what may be
  missed or read on resume, and do not replay completed effects.

**Runtime work:** source/connection validation and lifecycle support for each
shipped adapter. Superhuman login alone is not email ingestion. Independent
Flows, new adapters, and broader permissions remain separate runtime slices.

**Exit test:** configure one additional supported use case without hand-editing
YAML, observe its first authorized real Event in shadow, and review it. Failure
leaves the previous configuration and cursor intact. No unrelated scope expands.

### D6 — Higher-impact operations, only when the runtime supports them

Extend the established journeys, rather than build a second control center:

- Independent Flows and their health, revisions, and evaluation evidence once
  runtime routing and isolation exist.
- Agents with actual enforced capabilities, execution provenance, and terminal
  results. Configured capability hints are not proof of sandbox enforcement.
- Effect approval requests only after the runtime has a durable permission and
  approval protocol. Bind approval to the exact effect arguments and revision;
  expire stale requests and audit accept/deny. A review label is never approval.
- Retry/cancel controls only after the relevant adapter contract and idempotency
  rules exist. Never expose a generic "replay this Event" button to recover
  unknown effects.

No dates or broad integration promises are assigned to this milestone.

## 5. Write-access and data rules

These are prerequisites, not later hardening work:

- Preserve the existing read-only listener and APIs. Add a separately enabled,
  loopback-only control listener in the same binary, with an explicit private
  proxy route. Neither listener exposes generic ingestion through the dashboard.
- Require operator authentication and explicit allowed operations for controls,
  plus CSRF/Origin checks, bounded requests, and audit records. Reading a tailnet
  page alone must not grant mutation authority. Never trust arbitrary forwarded
  identity headers. The current implementation uses one environment-provisioned
  bearer token held only in browser memory, with explicit JSON write headers and
  Host/Origin checks. A multi-user role system is not implemented.
- Separate label writes, evaluation calls, configuration activation, promotion,
  and effect approvals. Confirmation is not a substitute for server validation.
  Connection tests use validated, configured targets and adapter-specific bounds,
  never an arbitrary URL fetcher or a browser-supplied command runner.
- Record actor, time, target, expected revision, and result for changes. Keep
  secrets and raw external responses out. Validation failures must not destroy
  the previous valid state.
- Keep Event evidence immutable. Use separate review, evaluation, and revision
  records with additive, transactional migrations and tested backup/recovery.
- Redaction does not make arbitrary Event text safe to publish. Protect exports
  and previews. Do not send private payloads to a new endpoint without approval.
- Keep query work bounded and avoid transactions across model/HTTP calls. Test
  a growing database and concurrent ingestion, not only a small empty fixture.

## 6. Interaction and release rules

- Preserve context on Back, refresh, and deep links. Refresh must not replace an
  unsaved review or configuration draft, move focus, or reorder the active item.
- Every task needs loading, empty, stale, failed, denied, conflict, and recovery
  states where applicable. Distinguish "not configured," "not observed," and
  "failed." An unavailable runtime cannot monitor or restart itself from its page.
- Phone layouts keep the Event, outcome, and evidence readable. Keyboard users
  can complete review and configuration with visible focus and labeled controls.
- Keep the current restrained visual identity. No node canvas, chat-first
  operator interface, decorative charts, public sharing, fleet management, or
  new frontend build stack is required by this roadmap.
- Test each shipped journey end to end. Include forbidden writes, hostile log
  text, exact IDs/numbers, interrupted jobs, stale revisions, and recovery. Add
  cross-browser and assistive-technology checks before relying on web controls.

Measure usefulness through task completion: diagnosis without SSH, representative
review coverage, corrections and unresolved cases, regressions found before
activation, and successful recovery. Track model latency and error rate alongside
quality, not as substitutes for it. Prefer local tests and operator sessions to
adding product analytics infrastructure.

## 7. Next implementation work

The original D1 → D2 sequence was superseded by the configuration-first request.
The next work should complete the remaining tasks, not replace the working
management interface with another framework:

1. Add reusable dataset selection and coverage checks. Keep explicit origin,
   uncertainty, label revisions, and tuning/validation boundaries.
2. Add effect-free source previews and pipeline checks. Do not present current
   interpreter evaluation as a test of filters, deduplication, or delivery.
3. Extend guided interpretation/filter/agent forms and connection checks only
   around supported runtime behavior and explicit consent.
4. Design evidence-gated live promotion separately. Agree acceptance limits
   before implementation; a passing evaluation must never enable effects itself.
5. Extend browser, keyboard, and assistive-technology checks and perform an
   authorized deployment/soak test. Local tests do not replace these checks.

Keep code boundaries in `internal/dashboard`, `internal/storage`,
`internal/runtime`, `internal/config`, and the CLI/control code in `cmd/mrmr`.
No roadmap item authorizes a source expansion, credential enrollment, production
migration, deployment, or live effect without a separate operator decision.
