# Agent-assisted setup

Use this runbook to configure and verify mrmr for someone. It is an installation guide, not a request to implement missing features. Contributor instructions live in [AGENTS.md](AGENTS.md).

**Success means a verified local installation with effects disabled—not a model certified for unattended action.**

## 1. Establish scope and inspect the environment

Read [IMPLEMENTATION.md](IMPLEMENTATION.md), then the [README](README.md) and [example configuration](mrmr.example.yaml). Inspect the current code when behavior is unclear; roadmap examples are not necessarily supported configuration.

Before changing anything:

- Check the OS, architecture, `go version`, and available disk space. The current module requires Go 1.27. Ask before installing or upgrading system software.
- Check for an existing binary, config, database, running process, service, and listener on the intended port. Do not stop another process or overwrite an installation to make the example commands work.
- Identify an existing model endpoint from information the user has provided or approved configuration. Do not scan networks or install a model server as an implicit setup step.
- Inspect only relevant files. Never dump the environment, credential files, or existing event databases into agent output.

Ask only for missing information:

1. **What should mrmr watch?** Start with a synthetic API event if the source is not decided.
2. **Which model endpoint and model ID should it use?** Confirm authorization to send data there, particularly for hosted endpoints that incur charges or receive private data.
3. **Where should the installation and data live?** Default to a user-owned local directory, not a system-wide installation.
4. **What should happen when something matters?** Record the eventual goal, but keep effects disabled during setup. Only stdout notifications are built in today; other destinations may require an explicitly configured HTTP integration.

If credentials are needed, ask the user to supply them through their environment or approved secret store, not chat. Record only the environment variable name and whether it is set. mrmr does not automatically load `.env` files.

**Stop rather than invent support.** Native MCP polling, provider-specific webhook normalization, XML RSS parsing, approval queues, and a web UI are not implemented. Explain the limitation and offer an existing ingestion path; do not build a bridge without a separate request.

## 2. Prepare an isolated candidate

Use a private scratch directory so verification cannot overwrite an existing configuration or write synthetic events into a real database. The examples below assume a POSIX shell in the repository root; adapt paths and commands for the user's platform. Keep the shell variables available across subsequent steps.

```bash
umask 077
setup_dir=$(mktemp -d "${TMPDIR:-/tmp}/mrmr-setup.XXXXXX")
binary="$setup_dir/mrmr"
config="$setup_dir/mrmr.yaml"
cp mrmr.example.yaml "$config"

go test ./...
go build -o "$binary" ./cmd/mrmr
```

Stop on any failed command. The tests do not need model credentials or a live endpoint; Go may need to download dependencies. Do not fix unrelated code as part of installation.

Edit the candidate, not the original example:

- `server.addr`: `127.0.0.1:4242`, or another confirmed-unused loopback port. The example's `:4242` binds all interfaces and is not a safe local default.
- `db.path`: the **absolute path** to `smoke.db` inside `setup_dir`. YAML does not expand shell variables or `~`; write the resolved path.
- `models.fast-local`: the approved `base_url`, exact model ID, and `api_key_env` name if needed. Omit `api_key_env` for an unauthenticated endpoint. Never place credentials in a URL or YAML value.
- Keep `interpret.model: fast-local` and the example prompt/schema for the initial connectivity check, unless the user has explicitly chosen another compatible schema.
- Remove active sources and agent targets (`sources: []`, `agents: {}`). Pollers run immediately at startup, even in shadow mode.
- Use `filter: []` so the synthetic event reaches the interpreter.
- Replace the policy and default with the following **temporary smoke-test policy**:

```yaml
policy: []
default:
  notify:
    via: stdout
    message: "{{ .result.summary }}"
  shadow: true
```

This deliberately makes every valid decision select a shadow notification, independent of classification accuracy. Failed interpretations still route to ignore. It tests the full successful path without executing an outcome adapter.

Do not add a duplicate `policy`, `default`, or other YAML key: replace the existing entries. Do not assume `default.shadow` globally disables effects; any matching rule with its own outcome must also carry `shadow: true`.

**Shadow still sends events to the model, stores data, and writes traces to logs.** Only synthetic, non-sensitive data belongs in this initial check.

## 3. Start temporarily and verify

There is no `init`, `doctor`, `status`, or standalone config-validation command. Startup validates the config. Use an owned foreground terminal/session for this temporary process:

```bash
"$binary" run -config "$config"
```

Keep track of that process so you can stop it with SIGINT after verification. Do not install a service or use an untracked background process. A startup log alone is not proof that the correct server is listening; confirm the owned process remains running and handles the request below.

In a second terminal, restore `setup_dir`, `binary`, and `config` to the resolved paths from step 2. Use the selected port if it differs from 4242:

```bash
api=http://127.0.0.1:4242
smoke_id="setup-$(basename "$setup_dir")"
printf '{"type":"setup.test","source":"setup-agent","subject":"synthetic-check","data":{"message":"Synthetic test: a fictional service has failed repeatedly. No real incident or action is requested."},"metadata":{"source_event_id":"%s"}}\n' \
  "$smoke_id" > "$setup_dir/event.json"

curl --silent --show-error --fail-with-body \
  --connect-timeout 5 --max-time 240 \
  -H 'Content-Type: application/json' \
  --data-binary "@$setup_dir/event.json" \
  "$api/api/events" > "$setup_dir/first-response.json"
```

Read the response privately and verify all of these:

- A new `event_id` exists and `duplicate` is absent or false.
- `decision.status` is `ok`, with fields matching the configured schema.
- `outcome` is `notify` under the temporary policy.
- The trace shows persistence, interpretation, default policy selection, and a **shadow** outcome.

HTTP 200 alone is insufficient: model failures also return successful HTTP responses with an errored/invalid decision and an ignore outcome. A mock endpoint passing this check does not verify the user's real model endpoint.

Copy the **first response's** event ID, then inspect it (replace the placeholder):

```bash
"$binary" inspect EVENT_ID_FROM_FIRST_RESPONSE -config "$config"
```

Confirm that SQLite contains the event, a successful decision, a `notify` execution with status `shadow`, and its trace. Do not treat an in-memory response as proof that the trace was persisted.

Send exactly the same event again:

```bash
curl --silent --show-error --fail-with-body \
  --connect-timeout 5 --max-time 240 \
  -H 'Content-Type: application/json' \
  --data-binary "@$setup_dir/event.json" \
  "$api/api/events" > "$setup_dir/duplicate-response.json"
```

Verify `duplicate: true`, a duplicate trace, and no decision or outcome. Inspect the original event again if needed; the duplicate response's newly generated event ID is **not** a stored row. The isolated database should still contain just one event:

```bash
"$binary" events -config "$config" -limit 10
```

Stop the owned process with SIGINT, restart it with the same config/database, and repeat the duplicate request. This checks that deduplication survives restart, not just a second request within one process. Stop it again when finished.

### If verification fails

- **Config rejected:** fix the named supported field in the candidate. Do not weaken validation or copy unimplemented keys from the roadmap.
- **Bind failure:** identify the existing listener; choose another loopback port rather than killing it.
- **Connection refused or HTTP 404:** check the approved endpoint, port, model API base path, and model ID. Include `/v1` when required by that server.
- **HTTP 401/403:** ask the user to correct credential provisioning. Check presence without printing the value.
- **Invalid/errored decision:** report the model check as failed. Inspect schema compatibility and endpoint availability; do not count fallback-to-ignore as success or enable effects to diagnose it.
- **Missing persisted records or failed restart:** stop promotion and investigate storage permissions/path and runtime errors. Never delete the database to make verification pass.

Do not paste raw endpoint errors into a handoff: they may include response bodies or sensitive details. Summarize a redacted error category. Do not retry indefinitely; after a corrected attempt still fails, report the blocker and ask for guidance.

## 4. Prepare the user's configuration

After the smoke test passes, prepare the intended installation in the agreed directory. Preserve any existing files and show the proposed config changes before replacing a working setup. Do not copy the smoke database into production or delete someone else's database.

- Install the verified binary to the agreed path without silently overwriting an existing binary.
- Give the final config an explicit loopback bind and an absolute, user-owned database path separate from `smoke.db`.
- Replace the temporary always-notify policy with rules appropriate to the user's goal and `default: {ignore: true}`. Keep `shadow: true` on every rule capable of effects until the user approves activation.
- Configure only the source the user requested. Starting it requires confirmation of the data access, model destination, and polling interval; sources fetch immediately and all sources share one pipeline.
- Keep files and directories holding config, logs, and data private to the user. Configuration should contain secret references, not values.

For an HTTP JSON source, follow the [polling configuration](README.md#http-polling). Confirm the record array path, stable ID field, and sample payload shape without exposing private records. IDs are dedup keys; changing a record without changing its ID does not emit an update. Failed or malformed records hold the watermark for retry; permanently malformed records require correcting the source. Cursor ordering/pagination is limited, and already-persisted partial processing has no recovery replay: do not promise lossless synchronization.

For Linux system-journal ingestion, use the [built-in source and shadow example](README.md#linux-system-journal), not a forwarding script or second service. Confirm the exact system-unit allowlist and severity. Check journal access as the service account; ask before changing group membership, and never run mrmr as root as a workaround. Startup begins at the current tail without backfill, so final verification needs a new authorized record after activation. User journals (including a Hermes user service) are not supported in this slice. A missing/vacuumed checkpoint is a blocker, not permission to silently reset history.

For API ingestion, the caller must send normalized events and supply stable `metadata.source_event_id` values. mrmr does not verify provider webhook signatures on their behalf.

Once the user has approved the final configuration and data access, start it temporarily and verify a representative authorized event reaches the intended filter/interpreter/policy path, remains shadowed (or ignored), and is inspectable in the final database. For a poller, verify its first fetch and a repeated poll without duplicate interpretation. If no representative event is available, report source verification as pending rather than claiming it works.

## 5. Ask before activation or persistence

Pause for explicit approval before:

- Removing shadow from any notification, HTTP action, emit-event, or delegation rule.
- Binding beyond loopback or changing firewall/network access. The API has **no authentication**, and callers can trigger configured policy.
- Installing/enabling a background service, autostart, or lingering; stopping/replacing an existing service; or making system-wide changes.

Show exactly which effects/targets, bind address, source schedule, or service would become active. There is no built-in permission or approval queue: these are human setup decisions, not commands mrmr can enforce later.

If persistent operation is approved, use the host's existing service manager; the README provides a [systemd example](README.md#deployment). Use absolute paths and explicitly provision credential references in the service environment. Shell exports are not automatically inherited by a service. Verify its startup, event trace, and stop/restart behavior; provide the exact service commands to the user.

Without approval, leave the installation configured but stopped. Stop only processes you started. Remove only your own disposable smoke artifacts after their results are recorded and the installed binary/config no longer depend on their paths.

## 6. Hand off with evidence

Provide a concise report with no secrets or private event payloads:

- **State:** configured/stopped, temporarily running, or service enabled; whether all effects remain shadowed.
- **Paths:** installed binary, final config, database, and service/log locations if applicable.
- **Commands:** exact start, stop, inspect, and restart commands for this installation. State that config changes require restart.
- **Verified:** build/tests, real model decision status, stored trace and shadow execution, duplicate suppression before/after restart, and final source verification (or explicitly pending).
- **Not verified / blockers:** unsupported integrations, credentials not provisioned, source not yet connected, or approval still required.
- **Next quality gate:** collect and label at least 50 representative real events and measure accuracy/false-ignore rate using the [evaluation workflow](README.md#inspect-label-and-evaluate). Agree acceptance thresholds with the user. Generated examples and this smoke test do not satisfy that gate; evaluation currently targets ignore/notify and skips pre-model filters.

Never summarize a successful synthetic check as “production ready.” The runtime still lacks crash-recovery replay, lossless polling, and a permission/approval layer. Make those limits visible before the user trusts unattended effects.
