#!/usr/bin/env python3
"""Opt-in real-model integration check. Owns all processes and uses only synthetic data.

Run under the resource-limited scope in README.md. No service is installed, no
source starts, and the temporary database/configuration are removed on exit.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

import yaml

from serve import model_id


def stop(process):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkpoint", type=Path, required=True)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--int8", action="store_true")
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cpu")
    args = parser.parse_args()
    recipe = Path(__file__).resolve().parent
    binary = str(args.binary.resolve())
    env = dict(os.environ, MRMR_D1_API_KEY=secrets.token_hex(32))
    # Never stop an existing listener to make a test work. Both processes will
    # also fail binding if another process wins the small check/start race.
    for port in (8091, 4242):
        with socket.socket() as sock:
            # Match HTTPServer/Go listener reuse: TIME_WAIT from our previous
            # check is not a live listener and must not block a second run.
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(("127.0.0.1", port))
    # Ignore HTTP proxy environment variables: synthetic local traffic and the
    # bearer token must never be sent through a configured external proxy.
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(url, body=None, auth=False):
        headers = {"Content-Type": "application/json"}
        if auth:
            headers["Authorization"] = "Bearer " + env["MRMR_D1_API_KEY"]
        req = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None, headers=headers)
        with http.open(req, timeout=180) as response:
            return json.load(response)

    def ready(process, port, model=False):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("owned process exited before ready")
            try:
                if model:
                    health = request(f"http://127.0.0.1:{port}/health", auth=True)
                    assert health["model"] == model_id(args.int8, args.device)
                else:
                    with socket.create_connection(("127.0.0.1", port), timeout=1):
                        pass
                return
            except (OSError, urllib.error.URLError):
                time.sleep(0.2)
        raise RuntimeError("owned process did not become ready")

    model_process = runtime_process = None
    with tempfile.TemporaryDirectory(prefix="mrmr-d1-smoke-") as tmp:
        # Logs contain only synthetic events. Keep them private and temporary;
        # never copy live journal data into this verification harness.
        os.chmod(tmp, 0o700)
        with open(Path(tmp) / "process.log", "w") as log:
            try:
                command = [sys.executable, str(recipe / "serve.py"), "--checkpoint", str(args.checkpoint.resolve()), "--device", args.device]
                if args.int8:
                    command.append("--int8")
                model_process = subprocess.Popen(command, env=env, stdout=log, stderr=log)
                ready(model_process, 8091, model=True)
                cfg = yaml.safe_load((recipe / "mrmr.example.yaml").read_text())
                cfg["models"]["d1"]["model"] = model_id(args.int8, args.device)
                cfg["db"]["path"] = str(Path(tmp) / "smoke.db")
                # Force only the synthetic smoke through a shadow notification.
                # This checks the successful pipeline, not classification quality.
                cfg["policy"] = []
                cfg["default"] = {"notify": {"via": "stdout"}, "shadow": True}
                config = str(Path(tmp) / "mrmr.yaml")
                Path(config).write_text(yaml.safe_dump(cfg))

                def start_runtime():
                    process = subprocess.Popen([binary, "run", "-config", config], env=env, stdout=log, stderr=log)
                    # The caller owns cleanup even if readiness fails.
                    return process

                runtime_process = start_runtime()
                ready(runtime_process, 4242)
                event = {
                    "type": "setup.test", "source": "d1-smoke", "subject": "synthetic-service",
                    "data": {"message": "Synthetic incident: the fictional service is unavailable and all writes fail."},
                    "metadata": {"source_event_id": "synthetic-1"},
                }
                started = time.perf_counter()
                result = request("http://127.0.0.1:4242/api/events", event)
                elapsed = time.perf_counter() - started
                assert result["decision"]["status"] == "ok", "model response was not accepted"
                assert result["decision"]["model"] == model_id(args.int8, args.device)
                assert result["outcome"] == "notify"

                def inspect():
                    return json.loads(subprocess.check_output([binary, "inspect", result["event_id"], "-config", config], env=env, stderr=log))

                stored = inspect()
                assert len(stored["decisions"]) == len(stored["executions"]) == 1
                assert stored["executions"][0]["status"] == "shadow"
                assert stored["trace"]
                assert request("http://127.0.0.1:4242/api/events", event)["duplicate"]
                stop(runtime_process)
                runtime_process = start_runtime()
                ready(runtime_process, 4242)
                assert request("http://127.0.0.1:4242/api/events", event)["duplicate"]
                assert len(inspect()["decisions"]) == 1
                print(json.dumps({"model": model_id(args.int8, args.device), "pipeline_seconds": elapsed,
                                  "decision": "ok", "execution": "shadow", "persisted_trace": True,
                                  "dedup_before_and_after_restart": True}), flush=True)
            finally:
                stop(runtime_process)
                stop(model_process)


if __name__ == "__main__":
    main()
