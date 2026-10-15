# Local d1 / System One candidate

Run LiquidAI's pinned d1-3B on CPU or CUDA behind mrmr's **existing** System One
HTTP contract. This recipe does not add a provider, change the default model,
start sources, enable effects, or install a background service.

**Status: integration verified on Marvin; NOT promoted.** See [RESULTS.md](RESULTS.md).
Full precision works but is slow. Experimental int8 is faster, changes judgments,
and missed too many notifications under the provisional policy. Neither variant
has passed the required 50+ labeled **real-event** gate.

## Boundary

```text
mrmr → POST /v1/systemone → local d1 → typed answers → mrmr validation/policy
```

`provider: typesafe` is the existing protocol selector, not a hosted-provider
requirement. The configured `base_url` determines the destination; this recipe
uses loopback. Other compatible servers can later use the same client. Merely
serving d1 through `/chat/completions` does not implement this decision interface.

The adapter adds the loaded model identity to d1's native `answers`/`usage`
response. It does not invent summaries, change probabilities, select outcomes,
fetch state URLs, or accept image/file inputs. `confidence` in this pinned d1
implementation is the largest option probability, not a separate certainty score.

Model identities include the revision **and precision variant**. The client must
request the exact loaded identity. No automatic model selection or cloud fallback.

## Prepare a fresh candidate

Requirements: Linux x86_64, Python 3.11 with venv, adequate RAM, and a built mrmr
binary. The recorded CPU dependency lock is not a promise of support on every
platform. Do not modify the system Python or an existing model environment.

From the repository root, choose an unused user-owned directory:

```sh
umask 077
work="$HOME/mrmr-d1-candidate"
mkdir "$work"                       # fails rather than overwriting a candidate
mkdir "$work/recipe"
cp recipes/local-d1/*.py recipes/local-d1/*.yaml \
  recipes/local-d1/*.lock recipes/local-d1/*.md "$work/recipe/"
python3.11 -m venv "$work/.venv"
"$work/.venv/bin/pip" install -r "$work/recipe/requirements.lock"
"$work/.venv/bin/pip" check

HF_HUB_DISABLE_IMPLICIT_TOKEN=1 "$work/.venv/bin/hf" download LiquidAI/d1-3B \
  --revision da1fe36a861f24690f27f622dca1d8688503d113 \
  --local-dir "$work/checkpoint"
```

The checkpoint is approximately 6.25 GB. It contains executable Python loaded via
`trust_remote_code=True`: review `api.py`, `modeling_d1.py`, `prompt.py`, `runner.py`,
`lfm2_vl.py`, and `hybrid.py` before running. Keep the checkpoint directory at the
pinned revision; don't replace its contents while retaining the old model ID.
Serving loads only local files in Hub offline mode, including the tokenizer.

Weights use the [LFM Open License](https://huggingface.co/LiquidAI/d1-3B/blob/main/LICENSE),
with a $10M annual-revenue commercial-use restriction, not an unrestricted
Apache/MIT license. No weights or upstream model code are bundled in mrmr.

## CUDA and private remote serving

`serve.py`, `benchmark.py`, and `smoke.py` accept `--device cuda`. This loads BF16
and reports the distinct model identity ending in `:cuda-bf16`. CUDA must be
available; there is no silent CPU fallback. `--int8` remains CPU-only. Keep the
CPU dependency lock separate from an ARM64/CUDA environment, and test each exact
runtime image. The same request and token bounds apply on GPU.

The server still binds to loopback by default. `--host` can explicitly select a
Tailscale IPv4 address for an approved remote deployment. Wildcard, public, and
ordinary LAN binds are rejected. Bearer authentication is required on every
endpoint. Keep the token in a private environment file or approved secret store,
never YAML. Tailnet addressing does not replace authentication or workload limits.

On a shared-memory GPU host, measure each model alone before testing them together.
Budget loading peaks, caches, and concurrent inference, not only checkpoint sizes.
Container memory limits do not account for every GPU-driver allocation.

## Run temporarily, with limits

On Marvin, first inspect available memory and Plex/Hermes/existing inference
activity. The initial fp32 load (also required before int8 conversion) peaked at
about **17.8 GiB RSS**. Do not run both variants simultaneously. Leave room for
other services; the 20 GiB cap below is specific to the measured 30 GiB host.

```sh
export MRMR_D1_API_KEY="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
cd "$work"
systemd-run --user --scope --unit=mrmr-d1-candidate \
  -p MemoryHigh=18G -p MemoryMax=20G -p MemorySwapMax=0 -p CPUQuota=400% \
  nice -n 10 timeout --kill-after=10 900 \
  .venv/bin/python recipe/serve.py --checkpoint checkpoint
```

This is a foreground, 15-minute candidate, **not** an enabled service. Ctrl-C
stops it. From another terminal, `systemctl --user stop mrmr-d1-candidate.scope`
stops the owned scope. A fresh start takes several seconds; int8 conversion adds
roughly 25 seconds. Never use a restart loop to conceal memory or model failures.

Supply the **same** `MRMR_D1_API_KEY` to mrmr's environment without printing it or
putting it in YAML. Authentication is required even on loopback. For persistent
operation, provision it via an approved secret mechanism after deployment review.

Copy `mrmr.example.yaml` into your candidate directory and set an absolute,
private database path. Start a built mrmr binary with that file:

```sh
/path/to/mrmr run -config "$work/mrmr.yaml"
```

All configured notifications are shadowed. No journal/poller is enabled. Source
allowlists and real-event data access need separate review before activation.

For the experimental int8 variant, add `--int8` to the serving command and change
the model ID suffix in YAML from `:cpu-fp32` to `:cpu-int8-channel`. This quantizes
only the language model's linear layers, per channel, retaining an fp32 output
head. It is **not** numerically equivalent to fp32 or a vendor-calibrated quant.
The PyTorch quantization API is deprecated; the tested version is pinned. Do not
upgrade it without repeating numerical checks and evaluation.

## Serving limits

- Loopback IPv4 by default, port 8091. An explicit Tailscale IPv4 bind is supported
  for approved remote deployment. Stdlib HTTP serving is not an internet edge.
- `GET /health` requires the same bearer token and reports the loaded model ID.
- `POST /v1/systemone`: exact `model`, `state`, `questions`; no other fields.
- At most 16 KiB JSON, 8 questions, and 1,024 **summed full-question prompt tokens**.
  The token sum deliberately overcounts shared state. Oversized input is rejected,
  never silently truncated; mrmr records failure and ignores it.
- One request at a time, small listen backlog, five-second socket I/O timeout.
  This is not a throughput server. A client timeout does not cancel native Torch
  work; the process-level resource/time limits above remain important.
- No request/response logging, raw model error echo, execution tools, or policy.
  mrmr still validates every returned probability and result field.

## Checks

Offline HTTP/auth/validation check, no Torch or weights required:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s recipes/local-d1 -v
```

After installing the checkpoint, run these **one at a time** under the same
resource-limited scope as above (replace the `serve.py` command):

```sh
.venv/bin/python recipe/benchmark.py --checkpoint checkpoint
.venv/bin/python recipe/benchmark.py --checkpoint checkpoint --int8
.venv/bin/python recipe/smoke.py --checkpoint checkpoint --binary /path/to/mrmr
.venv/bin/python recipe/smoke.py --checkpoint checkpoint --binary /path/to/mrmr --int8
```

The benchmark emits synthetic cold/warm timings, probabilities, Linux peak RSS,
and a token-ceiling check. It is not an accuracy benchmark. `smoke.py` owns and
stops its model/runtime processes, checks SQLite trace + shadow execution and
deduplication across restart, then removes its temporary synthetic database.
It refuses occupied ports; no other service is stopped.

For the repository's generated-data comparison, with the chosen server running
and a matching model variant in the candidate configuration:

```sh
/path/to/mrmr eval -config "$work/mrmr.yaml" -dataset testdata/generated-eval.jsonl
```

For a Jev comparison, copy that config and change only the model connection to
`base_url: https://api.typesafe.ai/v1`, `api_key_env: TYPESAFE_API_KEY`, and
`model: jev-1.13.0`. Hosted evaluation sends event content to TypeSafe; the recorded
comparison used **only generated synthetic fixtures**, not journal/email data.
Use identical questions/policy and record validation failures separately from
judgment errors. Score thresholds need per-model calibration, not blind copying.

## Promotion remains a human decision

Collect at least 50 representative, labeled real events with approved source
access. Measure missed notifications, false alerts, validity, latency, and host
contention. Include adversarial/noisy inputs. Test the exact precision variant;
quantized probabilities cannot inherit full-precision thresholds.

Agree acceptance thresholds before tuning. Do not tune to this generated set and
call it a real-event gate. Keep effects shadowed and the global default unchanged
until reviewed. Persistent service installation, network exposure, and source
activation are also separate approval steps.
