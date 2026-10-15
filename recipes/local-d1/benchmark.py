#!/usr/bin/env python3
"""Bounded synthetic timings through the same decider used by the HTTP server.

Not a quality evaluation. Never substitutes for labeled real events. Load and
warm timings are separate, and Linux peak RSS includes model loading/conversion.
"""
import argparse
import json
from pathlib import Path
import resource
import statistics
import time

from serve import load_decider, model_id


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkpoint", type=Path, required=True)
    parser.add_argument("--int8", action="store_true")
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cpu")
    args = parser.parse_args()
    started = time.perf_counter()
    decide = load_decider(args.checkpoint.resolve(), 4, args.int8, args.device)
    print(json.dumps({"model": model_id(args.int8, args.device), "load_seconds": time.perf_counter() - started}), flush=True)
    questions = {
        "category": {"type": "choice", "instructions": "Which category best describes this system service event?", "criteria": {
            "incident": "A service is broken, unavailable, repeatedly failing, or losing data now.",
            "noise": "Routine startup, shutdown, retry, or warning chatter needing no attention.",
            "FYI": "Useful operational information that is neither an incident nor routine noise.",
        }},
        "requires_action": {"type": "noul", "instructions": "Does the operator genuinely need to take action?"},
        "severity": {"type": "score", "instructions": "How severe is the operational impact?", "criteria": ["No impact", "Degraded with workaround", "Unavailable or data at risk"]},
    }
    short = "The service has failed repeatedly and is unavailable."
    for name, state, qs in [
        ("one", short, {"category": questions["category"]}),
        ("three", short, questions),
        ("longer", "Routine heartbeat: the service is healthy. " * 20 + "Now the disk is full and all writes fail.", questions),
    ]:
        times = []
        for _ in range(4):
            start = time.perf_counter()
            result = decide(state, qs)
            times.append(time.perf_counter() - start)
        assert result["usage"]["output_tokens"] == 0
        assert set(result["answers"]) == set(qs)
        print(json.dumps({
            "workload": name, "cold_seconds": times[0],
            "warm_median_seconds": statistics.median(times[1:]), "warm_seconds": times[1:],
            "usage": result["usage"], "answers": result["answers"],
            "peak_rss_kib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
        }, allow_nan=False), flush=True)
    if args.device == "cuda":
        import torch
        # RSS alone misses device allocations, especially on unified-memory hosts.
        print(json.dumps({"cuda_peak_allocated_bytes": torch.cuda.max_memory_allocated(),
                          "cuda_peak_reserved_bytes": torch.cuda.max_memory_reserved()}), flush=True)
    # Ensure an over-limit request fails before spending inference, not after
    # the runtime's HTTP timeout. This checks the real pinned tokenizer path.
    try:
        decide("heartbeat " * 2000, questions)
    except ValueError:
        print('{"over_limit_rejected": true}', flush=True)
    else:
        raise AssertionError("token ceiling was not enforced")


if __name__ == "__main__":
    main()
