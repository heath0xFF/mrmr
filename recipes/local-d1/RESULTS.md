# Marvin candidate results

These are feasibility and integration results, **not a promotion decision**.
No real journal/email data was read or sent to a hosted model. No default was
changed, no sources were enabled, and no persistent service was installed.

## Environment

- Marvin: Intel Core Ultra 9 185H, AVX2, 22 logical CPUs, approximately 30 GiB RAM.
- Before testing: about 26 GiB available, existing inference idle; Plex,
  `lfm25.service`, and the Hermes user service active.
- Isolated Python 3.11 environment; Torch 2.14.1+cpu, Transformers 5.19.0.
  Full dependency versions are in `requirements.lock`.
- `LiquidAI/d1-3B`, revision `da1fe36a861f24690f27f622dca1d8688503d113`.
- Four Torch intra-op threads, one inter-op thread, nice 10, CPUQuota 400%,
  MemoryHigh 18 GiB, MemoryMax 20 GiB, MemorySwapMax 0. Sequential tests only.
- No CPU affinity or competing-load stress test. Availability checks are not
  proof that Plex playback or Hermes responsiveness was unaffected under load.

## Feasibility

Initial unquantized CPU test, warm medians over three runs:

| Workload | Input tokens reported | fp32 |
| --- | ---: | ---: |
| One question, short synthetic incident | 85 | 6.41 s |
| Three questions, same incident | 148 | 11.12 s |

The initial longer-state test (400 repeated heartbeat sentences) did not finish
its four-run workload before the overall 600-second process limit. There is no
per-request long-input latency result from that run. This prompted an explicit,
conservative CPU token ceiling in the serving recipe.

Naive whole-model dynamic int8 was faster, but changed `requires_action.noul`
from **0.819 to 0.402** on the same three-question synthetic incident. Rejected
as a serving variant; its preliminary script was removed with the private
candidate directory during cleanup.

The shipped **experimental** variant uses per-channel dynamic quantization of
language-model linear layers and leaves the output head in fp32. The same Noul
became **0.761**, still not equivalent to fp32. Final `benchmark.py --int8` run:

| Workload | Input tokens reported | Warm median (three runs) |
| --- | ---: | ---: |
| One question | 85 | 0.555 s |
| Three questions | 148 | 1.015 s |
| Three questions, 20 heartbeat sentences + failure | 329 | 2.072 s |

- Load + conversion: 27.5 s in that run.
- Peak RSS, including initial full-precision loading: about 17.8 GiB for both
  variants. Int8 ready-state RSS in the evaluation run: about 6.7 GiB.
- Over-limit input rejected before inference using the pinned tokenizer.
- Small samples, not tail-latency guarantees or comparisons against GPU claims.

## Real-model integration

`smoke.py` passed on Marvin for **both** variants using the unchanged Go client:

- Authenticated local System One request with Choice, Noul, and Score.
- Validated Decision with pinned precision-specific model identity.
- Persisted Event, Decision, execution, and trace in SQLite.
- Notification recorded as `shadow`, not executed.
- Duplicate suppressed before and after restarting mrmr; still one Decision.
- Owned processes stopped and temporary synthetic database removed.

Single synthetic end-to-end event: **21.70 s fp32**, **1.89 s int8-channel**.
These are pipeline measurements, not directly comparable to the shorter
benchmark prompts above. fp32 remains the recipe default; int8 is explicit opt-in.

## Generated-set comparison

Same 50 cases from `testdata/generated-eval.jsonl`, same questions and provisional
policy from this recipe, with only the model connection changed. Both category
confidence and action probability gates were `>= 0.85`. This is a measurement of
**model + validation + untuned policy**, not a pure model accuracy leaderboard.

| Metric | d1 CPU int8-channel | Jev 1.13.0 (hosted) |
| --- | ---: | ---: |
| Valid decisions | 50/50 | 40/50 |
| Category correct | 30/50 | 33/50 |
| Requires-action correct (Noul >= 0.5) | 48/50 | 40/50 |
| Outcome correct | 36/50 | 36/50 |
| False ignores among expected notifications | **14/18** | **11/18** |

The d1 run took 89.2 seconds excluding server startup. All expected-ignore cases
routed to ignore, but that is little comfort when most expected notifications
were also ignored. Several correct category/action judgments were suppressed
by the untuned 0.85 gates; category errors also occurred, including `smoke-alarm`
classified as noise. No thresholds were tuned against these fixtures.

Jev's ten invalid decisions confound a quality comparison: nine Score answers
failed the current probability-weighted consistency check (reported differences
at the 0.01 rounding boundary), and one Choice disagreed with its highest reported
probability. Validation was **not weakened** to force the comparison through.
The rounding-boundary handling needs separate investigation and a regression
check before repeating the comparison. Invalid outputs correctly routed to ignore.

The full fp32 checkpoint has **not** been run across this 50-case set. Do not
attribute quantized quality results to the full-precision checkpoint.

## Remaining gates / cleanup

At the user's request, `~/mrmr-d1-candidate/` was removed from Marvin, including
its isolated environment, checkpoint, binary, scripts, and synthetic logs
(7.1 GB). Test processes, listeners, scopes, and temporary smoke databases had
already been cleaned up; no autostart unit was installed. Plex, Hermes, and
existing inference remained active after removal. This repository retains the
recipe and summarized results. Smoke tokens were ephemeral process environment
values, not written into configuration.

Before promotion:

1. Investigate Jev's response-consistency failures separately; repeat the baseline.
2. Collect 50+ representative labeled **real** events with approved data access.
3. Compare exact model variants, tune on separate data, and agree acceptable
   false-ignore/false-alert rates before enabling effects.
4. Measure latency/headroom while Marvin's actual workloads are active.
5. Review source activation and any persistent deployment independently.

Keep the project default unchanged. The reusable System One integration works;
the judgment quality and deployment economics have not earned promotion yet.
