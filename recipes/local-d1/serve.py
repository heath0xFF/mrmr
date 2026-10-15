#!/usr/bin/env python3
"""A private d1 server for mrmr's existing System One client.

This is a serving recipe, not another runtime or provider framework. It accepts
text/JSON state only, never fetches URLs, and leaves response validation and all
policy authority in mrmr. No prompts, events, tokens, or answers are logged.
"""

import argparse
import hmac
import importlib
import ipaddress
import json
import os
from pathlib import Path
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

REVISION = "da1fe36a861f24690f27f622dca1d8688503d113"
MODEL = f"LiquidAI/d1-3B@{REVISION}"
MAX_BODY = 16 * 1024
# A conservative serving ceiling, not the model's advertised 32K context. Counting each
# question's full prompt overcounts the shared state, deliberately conservatively.
MAX_TOKENS = 1024
ID = re.compile(r"[A-Za-z0-9_-]+\Z")


def model_id(int8=False, device="cpu"):
    # Precision changes probabilities; each tested variant needs its own identity.
    if device not in ("cpu", "cuda") or (int8 and device != "cpu"):
        raise ValueError("int8 is CPU-only; device must be cpu or cuda")
    if device == "cuda":
        return MODEL + ":cuda-bf16"
    return MODEL + (":cpu-int8-channel" if int8 else ":cpu-fp32")


def private_host(value):
    # Remote deployment is tailnet-only; never expose this stdlib server publicly.
    address = ipaddress.IPv4Address(value)
    if not (address.is_loopback or address in ipaddress.IPv4Network("100.64.0.0/10")):
        raise ValueError("listen address must be loopback or a Tailscale IPv4 address")
    return str(address)


def reject_constant(_):
    raise ValueError("non-finite JSON number")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def validate_request(value, expected_model):
    """Reject malformed input before invoking upstream custom model code.

    These checks mirror mrmr's question subset, plus serving resource limits.
    The HTTP boundary needs them even when the usual caller already validates.
    """
    if not isinstance(value, dict) or set(value) != {"model", "state", "questions"}:
        raise ValueError("expected model, state, and questions")
    if value["model"] != expected_model:
        raise ValueError("model does not match loaded variant")
    questions = value["questions"]
    if not isinstance(questions, dict) or not 1 <= len(questions) <= 8:
        raise ValueError("expected 1 to 8 questions")
    for name, question in questions.items():
        if not ID.fullmatch(name) or not isinstance(question, dict):
            raise ValueError("invalid question")
        if not {"type", "instructions"} <= set(question) <= {"type", "instructions", "criteria"}:
            raise ValueError("invalid question fields")
        instructions = question["instructions"]
        if not isinstance(instructions, str) or not instructions.strip():
            raise ValueError("instructions are required")
        kind, criteria = question["type"], question.get("criteria")
        if kind == "noul" and criteria is None:
            continue
        if kind in ("choice", "noul"):
            if not isinstance(criteria, dict):
                raise ValueError("criteria must be an object")
            if kind == "choice" and not 2 <= len(criteria) <= 255:
                raise ValueError("choice needs 2 to 255 options")
            if kind == "noul" and set(criteria) != {"true", "false"}:
                raise ValueError("noul needs true and false criteria")
            if any(not ID.fullmatch(key) for key in criteria):
                raise ValueError("invalid criterion name")
            descriptions = criteria.values()
        elif kind == "score":
            if not isinstance(criteria, list) or not 2 <= len(criteria) <= 10:
                raise ValueError("score needs 2 to 10 levels")
            descriptions = criteria
        else:
            raise ValueError("unsupported question type")
        if any(not isinstance(text, str) or not text.strip() for text in descriptions):
            raise ValueError("criteria need nonempty descriptions")
    return value


def load_decider(checkpoint, threads, int8=False, device="cpu"):
    # Downloads are an explicit setup step. Loading by local path also pins the
    # tokenizer that upstream lazily loads without forwarding a Hub revision.
    # Offline mode prevents request data from triggering any Hub/network access.
    model_id(int8, device)  # Reject unsupported variants before loading weights.
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"
    os.environ["TOKENIZERS_PARALLELISM"] = "false"
    import torch
    from transformers import AutoModel

    if device == "cuda" and not torch.cuda.is_available():
        raise RuntimeError("CUDA is required; refusing a silent CPU fallback")
    torch.set_num_threads(threads)
    torch.set_num_interop_threads(1)
    model = AutoModel.from_pretrained(
        str(checkpoint), trust_remote_code=True, local_files_only=True,
        dtype=torch.bfloat16 if device == "cuda" else torch.float32,
    ).eval().to(device)
    if int8:
        # Experimental: per-channel language-model weights, with the output head
        # left in fp32. Whole-model default int8 flipped a synthetic yes/no answer.
        # This variant also shifts probabilities and needs its own quality gate.
        from torch.ao.quantization import quantize_dynamic, per_channel_dynamic_qconfig
        quantize_dynamic(
            model.model.language_model,
            {torch.nn.Linear: per_channel_dynamic_qconfig}, inplace=True,
        )
    # Use the pinned model's own prompt renderer to enforce the token ceiling;
    # estimating from characters would undercount many Unicode inputs.
    prompt = importlib.import_module(model.__class__.__module__.rsplit(".", 1)[0] + ".prompt")
    engine = model.engine

    def decide(state, questions):
        tokens = sum(engine.tokens(state, [prompt.as_question(q)]) for q in questions.values())
        if tokens > MAX_TOKENS:
            raise ValueError("request token limit exceeded")
        return model.system_one(state, questions)

    return decide


class DecisionServer(HTTPServer):
    # ponytail: one request at a time, one small listen backlog. Add bounded
    # admission/workers only if real throughput requires it; never share this
    # mutable model across unbounded request threads.
    request_queue_size = 1

    def __init__(self, address, decide, token, identity):
        self.decide, self.token, self.identity = decide, token, identity
        super().__init__(address, Handler)

    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(5)  # Slow/incomplete HTTP bodies cannot park the server forever.
        return connection, address

    def handle_error(self, request, client_address):
        # BaseServer's traceback can include model exceptions containing state.
        print("d1: request failed", flush=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass  # Even URLs and headers are caller-controlled and potentially sensitive.

    def reply(self, status, body):
        encoded = json.dumps(body, allow_nan=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(encoded)
        self.close_connection = True

    def authorized(self):
        values = self.headers.get_all("Authorization", [])
        expected = ("Bearer " + self.server.token).encode()
        if len(values) != 1 or not hmac.compare_digest(values[0].encode(), expected):
            self.reply(401, {"error": "unauthorized"})
            return False
        return True

    def do_GET(self):
        if self.authorized():
            if self.path == "/health":
                self.reply(200, {"model": self.server.identity, "status": "ready"})
            else:
                self.reply(404, {"error": "not found"})

    def do_POST(self):
        if not self.authorized():
            return
        if self.path != "/v1/systemone":
            self.reply(404, {"error": "not found"})
            return
        try:
            lengths = self.headers.get_all("Content-Length", [])
            if len(lengths) != 1 or "Transfer-Encoding" in self.headers:
                raise ValueError("one content length required")
            size = int(lengths[0])
            if not 0 < size <= MAX_BODY:
                self.reply(413, {"error": "body limit exceeded"})
                return
            if self.headers.get_content_type() != "application/json":
                self.reply(415, {"error": "application/json required"})
                return
            body = self.rfile.read(size)
            if len(body) != size:
                raise ValueError("incomplete body")
            value = json.loads(body, parse_constant=reject_constant, object_pairs_hook=unique_object)
            # Also reject overflowing JSON numbers (1e999), which parse_constant
            # does not see, before passing any state to the model.
            json.dumps(value, allow_nan=False)
            validate_request(value, self.server.identity)
        except (ValueError, UnicodeError, RecursionError, TimeoutError):
            self.reply(400, {"error": "invalid request"})
            return
        try:
            result = self.server.decide(value["state"], value["questions"])
            self.reply(200, {**result, "model": self.server.identity})
        except ValueError:
            self.reply(400, {"error": "request rejected by model"})
        except Exception:
            # mrmr receives a bounded failure and routes to ignore. Never echo
            # upstream exceptions: they can contain private state or prompts.
            self.reply(500, {"error": "inference failed"})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkpoint", type=Path, required=True)
    parser.add_argument("--port", type=int, default=8091)
    parser.add_argument("--host", default="127.0.0.1", help="loopback or Tailscale IPv4 only")
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cpu")
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--int8", action="store_true", help="experimental; changes probabilities")
    args = parser.parse_args()
    token = os.environ.get("MRMR_D1_API_KEY", "")
    if len(token) < 32 or not token.isascii() or any(c.isspace() for c in token):
        parser.error("MRMR_D1_API_KEY must contain at least 32 non-whitespace ASCII characters")
    if not args.checkpoint.is_dir() or not 1 <= args.threads <= 22 or not 1 <= args.port <= 65535:
        parser.error("invalid checkpoint, thread count, or port")
    try:
        host, identity = private_host(args.host), model_id(args.int8, args.device)
    except ValueError as error:
        parser.error(str(error))
    decide = load_decider(args.checkpoint.resolve(), args.threads, args.int8, args.device)
    with DecisionServer((host, args.port), decide, token, identity) as server:
        print(f"d1 ready on {host}:{args.port} ({server.identity})", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
