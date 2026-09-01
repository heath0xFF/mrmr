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
- OpenAI-compatible model interpretation
- schema-constrained output with validation and bounded retries
- deterministic first-match policy
- stdout notification and ignore outcomes
- event traces and deduplication

The required 50-event golden-set quality gate has not yet been completed.

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

The response contains the persisted decision, selected outcome, and complete trace.

## Development

```bash
go test ./...
go build ./...
```

See [IMPLEMENTATION.md](IMPLEMENTATION.md) for the architecture, milestones, and scope.

## License

[MIT](LICENSE)
