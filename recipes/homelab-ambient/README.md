# Homelab ambient investigation

Observe → judge → policy → investigate only when needed → observe the result → judge again.

[The complete configuration](mrmr.example.yaml) uses a local d1 decision model
and an external HTTP investigator backed by LFM2.5-2.6B. Neither model name has
special meaning in runtime policy. Every effect is **shadowed by default**.
This is a recipe, not a bundled agent or a verified real-event quality gate.

## What runs

1. The built-in journal source selects warning-or-higher records from one exact system unit.
2. mrmr persists and deduplicates the event, then asks the judge for a Choice and Yes/No judgment.
3. Routine or resolved events are ignored. Urgent events select a notification.
4. A confident `needs_investigation` judgment selects the configured `homelab` agent.
5. When enabled, that endpoint returns a structured terminal result in the same HTTP request.
6. mrmr stores `agent.investigation.completed` with parent/decision/execution references and depth + 1.
7. Exact event-type selection chooses the follow-up questions. Policy can ignore or notify for human review. This recipe never delegates a completion and never restarts a service.

The runtime is the root. The external worker is an effect, not a controller.

## Prepare safely

- Follow [SETUP.md](../../SETUP.md). Use an isolated database and synthetic events first.
- Prepare the judge with [local-d1](../local-d1/README.md), or use an existing approved System One endpoint. The `liquid-d1` selector uses that recipe's `/v1/systemone` contract. Pin the exact model revision and precision. Liquid also documents a hosted System One API: use `base_url: https://api.liquid.ai/decisions/v1`, `model: d1:free`, and `api_key_env: LIQUID_API_KEY`. Hosted use sends event data off-machine; its transport is contract-tested here, not live-tested.
- To use Jev, change the judge provider to `typesafe`, its base URL to `https://api.typesafe.ai/v1`, model to the approved Jev version, and secret reference to `TYPESAFE_API_KEY`. This sends event content to an external service.
- To use a local chat model as judge, select `openai-compatible` and its base URL/model. The same questions work, but this path does **not** supply probabilities or confidence. Confidence-gated rules will not match. Review and evaluate a choice-only policy; do not invent confidence values to activate it.
- Supply your own external agent endpoint. Its model client must use the configured worker endpoint/model and its own approved credentials. mrmr sends no model API key to the agent.
- Enforce the read-only contract outside the model: fixed tool names, allowed services, bounded tool calls/time, read-only credentials, and no mutation tools. Reject unsupported capability hints. A prompt or `read_only: true` is **not a sandbox**.
- Review journal access and unit names before starting. For a portable API-only smoke test, replace `sources` with `[]`. Shadow still calls the judge and stores event content.

Build and run your edited copy:

```sh
go build -o mrmr ./cmd/mrmr
./mrmr run -config /path/to/your/ambient.yaml
```

Do not remove shadow from all rules at once. After approval, enable only the
read-only delegation rule to test the actual completion loop. Leave notifications
shadowed until reviewed. A shadow delegation makes no agent call and therefore
cannot produce a completion event. You can submit a synthetic completion-shaped
event through the ordinary API to check the follow-up interpreter before then;
this is not proof of authentic worker provenance.

## External agent contract

The endpoint receives the existing `event_id`, `decision`, and static `prompt`,
plus `event`, `delegation`, `depth`, `emit_completion`, `read_only`, `capabilities`,
and an optional `worker` object with provider/base URL/model. `delegation` includes
origin and parent event IDs, decision ID, execution ID, policy rule/hash, and agent
name. Use the execution ID as an idempotency key where possible.

With `emit_completion: true`, return HTTP 200 and one JSON object, at most 1 MiB:

```json
{
  "status": "completed",
  "summary": "The service recovered after a transient storage timeout.",
  "findings": ["Service is active; no further errors in the observed window."],
  "evidence": [{"source": "journal", "record_id": "example-cursor"}],
  "recommended_action": "no_action",
  "metadata": {"checks": 2},
  "started_at": "2026-01-01T12:00:00Z",
  "completed_at": "2026-01-01T12:00:02Z"
}
```

Only `status` is required: `completed`, `failed`, or `cancelled`. The other fields
are optional. Findings/evidence are arrays; metadata is an object. Unknown
outer fields are rejected. Evidence URLs are data and are never fetched.
Timestamps, if present, must be RFC3339 and ordered. Failed/cancelled work also
creates an observation; it does not grant a remediation permission.

The adapter makes one request, with a **60-second deadline in this recipe**, no
effect retries, and no redirects. `agents.<name>.timeout` accepts 1s through 5m;
omitting it retains the original 10-second bound. A timeout or malformed response records an execution error and
emits no child. HTTP 202 is not a completion; callback delivery is not implemented.
For longer work, keep `emit_completion` omitted/false and use the existing 2xx
acceptance mode. Do not point this endpoint at `/chat/completions`: an agent
harness must implement this task contract and enforce tool limits.

## Authority and provenance

The result becomes `event.data.result`; the configured agent name is
`event.data.agent`. The runtime sets event type, source, subject, IDs, timestamps,
dedup key, depth, and provenance. The agent cannot replace those fields.
`recommended_action` is only semantic evidence. A policy may map a value such as
`restart_service` to a fixed configured HTTP action URL; it must not template a
URL, method, tool name, or permission from the result. This recipe deliberately
stops at human notification because mrmr has no approval queue yet.

`mrmr inspect EVENT_ID -config ...` shows child IDs on the parent and lineage on
the child. Execution intent is stored before dispatch. Depth greater than 10 is
stored but ignored without a model call. The recipe's depth/type policy is
stricter: only an initial journal event can delegate.

Keep the API private. It has no authentication; callers can submit arbitrary
source/type values. The API does not accept runtime provenance or depth, but a
completion-shaped API payload is not proof of an authentic investigation.
There is no queue replay, asynchronous callback, or exactly-once guarantee.

## Evaluate before promotion

Use `category` for the Choice question in both stages and `requires_action` for
the optional Yes/No question. Label both journal and completion events normally:

```sh
./mrmr label EVENT_ID -config /path/to/ambient.yaml \
  -category needs_investigation -requires-action=true -outcome delegate
./mrmr label COMPLETION_ID -config /path/to/ambient.yaml \
  -category no_action -requires-action=false -outcome ignore
./mrmr dataset export -config /path/to/ambient.yaml -output /private/real-eval.jsonl
./mrmr eval -config /path/to/ambient.yaml -dataset /private/real-eval.jsonl
```

Evaluation selects the same event-type question set and policy, but never invokes
an agent or effect. The [synthetic fixture](eval.jsonl) checks both shapes; it is
not a quality benchmark. Use separate initial-event and completion-event datasets
to compare each stage. `-mode decision` or `-mode structured-output` selects only that contract before
inference. Reports identify decision vs structured-output calls,
accuracy, class precision/recall, confusion, probability buckets, and false-ignore
rate. Probability buckets use the selected choice's probability, **not confidence**.
Missing probabilities produce no calibration samples.

Collect at least 50 representative real labeled events, including failed work,
missing evidence, and hostile log text. Set a false-ignore acceptance limit before
tuning. Re-evaluate each model revision/precision and each policy change.
