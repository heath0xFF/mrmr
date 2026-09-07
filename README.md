# mrmr

**A local-first, event-driven runtime for ambient AI.** Pronounced “murmur.”

mrmr turns outside-world events into structured model decisions, applies deterministic policy, and selects a controlled outcome:

```text
                 ┌→ ignore
                 ├→ notify
event → interpret├→ act
                 └→ delegate
```

> **LLM = judgment. Runtime = authority.**

Models interpret events. Ordinary code decides what is allowed to happen.

## Status

mrmr is early-stage software. The initial vertical slice is implemented:

- `POST /api/events`
- SQLite persistence for events, decisions, and executions
- deterministic pre-model filters (an event that fails any check is recorded and ignored before the model is called)
- OpenAI-compatible model interpretation
- schema-constrained output with validation and bounded retries
- deterministic first-match policy
- stdout notification, ignore, HTTP action, emit-event (depth-capped), and generic-HTTP delegate outcomes, plus shadow mode (outcome recorded, nothing executed)
- event traces and deduplication, with every trace persisted and queryable by `mrmr inspect EVENT_ID`

A generated 50-event evaluation set is included for prompt development. The required real-event golden-set quality gate has not yet been completed.

## Run locally

Requirements: Go 1.27 and an OpenAI-compatible model endpoint.

```bash
cp mrmr.example.yaml mrmr.yaml
# Edit mrmr.yaml for your endpoint and model.
export MRMR_MODEL_API_KEY='your-local-endpoint-key'
go run ./cmd/mrmr run -config mrmr.yaml
```

Send an event:

```bash
curl http://localhost:4242/api/events \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "test.message",
    "source": "curl",
    "data": {
      "message": "The production API has returned 500 errors for five minutes."
    }
  }'
```

The response always contains the complete trace; events that reach policy also carry the selected outcome, interpreted events carry the persisted decision, and duplicates carry a `duplicate` marker.

## Evaluate a model

Run the generated labeled set against the configured model:

```bash
go run ./cmd/mrmr eval -config mrmr.yaml -dataset testdata/generated-eval.jsonl
```

Progress is printed to stderr. The JSON summary reports category, action, and outcome accuracy plus the false-ignore rate. Generated cases are a development baseline, not a substitute for real dogfood events.

## Capture and label real events

Every ingested event is already stored in SQLite. Review the unlabeled queue, attach the expected judgment, and export it in the same format used by `mrmr eval`:

```bash
go run ./cmd/mrmr events -config mrmr.yaml -unlabeled
go run ./cmd/mrmr label evt_01... -config mrmr.yaml \
  -category incident -requires-action=true -outcome notify
go run ./cmd/mrmr dataset export -config mrmr.yaml \
  -output testdata/real-eval.jsonl
go run ./cmd/mrmr eval -config mrmr.yaml \
  -dataset testdata/real-eval.jsonl
```

Relabeling an event replaces its previous label. Exports contain complete event payloads and are ignored by Git by default; inspect and anonymize them before sharing or committing.

## Inspect an event

The ingest response carries the trace, but it is gone once the caller drops it. Every trace is also stored, so any event can be explained afterwards:

```bash
go run ./cmd/mrmr inspect evt_01... -config mrmr.yaml
```

The output is one JSON object with the stored event, its decisions, its executions, the ordered trace, and the human label if the event has one. Duplicates are the exception: a redelivery is never stored a second time, so the original event's trace is the explanation for both.

## Deploy to a home server

mrmr is a single static binary with SQLite storage, so the typical deployment is one small always-on box (home server, mini PC, Raspberry Pi) plus any OpenAI-compatible model endpoint already on the network:

```bash
# Build for the target from anywhere.
GOOS=linux GOARCH=amd64 go build -o mrmr ./cmd/mrmr
scp mrmr mrmr.yaml home-server:~/mrmr/
```

Point the config at the model server and keep the HTTP API tailnet- or LAN-only — the runtime has no authentication:

```yaml
server:
  addr: 100.x.y.z:4242   # bind to the machine's tailnet/LAN address

db:
  path: /home/you/mrmr/mrmr.db

models:
  fast-local:
    provider: openai-compatible
    base_url: http://model-server:8000/v1
    model: local-model
```

Run it as a systemd user unit:

```ini
# ~/.config/systemd/user/mrmr.service
[Unit]
Description=mrmr ambient event runtime
After=network-online.target

[Service]
ExecStart=%h/mrmr/mrmr run -config %h/mrmr/mrmr.yaml
Restart=on-failure

[Install]
WantedBy=default.target
```

Until the built-in poller lands, real events come from anything that can POST — a cron job checking a service every minute is enough to start:

```bash
* * * * * curl -sf -m 5 http://model-server:8000/health >/dev/null || \
  curl -sf http://home-server:4242/api/events -H 'Content-Type: application/json' \
       -d '{"type":"service.unhealthy","source":"cron","subject":"model-server","data":{"service":"model-api"}}'
```

Events accumulate in SQLite, get labeled with `mrmr label`, and grade the interpreter via `mrmr eval` — the deployment doubles as the golden-set collector.

## Development

```bash
go test ./...
go build ./...
```

See [IMPLEMENTATION.md](IMPLEMENTATION.md) for the architecture, milestones, and scope.

## License

[MIT](LICENSE)
