"""Offline HTTP boundary checks; no Torch, weights, credentials, or network service."""

import copy
import http.client
import json
import threading
import unittest

from serve import DecisionServer, MAX_BODY, model_id


class ServingTest(unittest.TestCase):
    def test_http_contract_and_rejection(self):
        # Own and join the server thread even on assertion failure. The fake
        # only supplies inference; serialization/auth/validation use real HTTP.
        calls = []
        fail = False
        identity = model_id()
        token = "synthetic-test-token-not-a-secret-1234"

        def decide(state, questions):
            if fail:
                raise RuntimeError("private model state must not escape")
            calls.append((state, questions))
            return {
                "answers": {
                    "category": {"type": "choice", "choice": "incident", "confidence": 0.9,
                                 "probabilities": {"incident": 0.9, "noise": 0.1}},
                    "action": {"type": "noul", "noul": 0.8},
                    "severity": {"type": "score", "score": 0.8, "confidence": 0.8,
                                 "probabilities": {"0": 0.2, "1": 0.8},
                                 "legend": {"0": "Low", "1": "High"}},
                },
                "usage": {"input_tokens": 100, "output_tokens": 0},
            }

        server = DecisionServer(("127.0.0.1", 0), decide, token, identity)
        worker = threading.Thread(target=server.serve_forever)
        worker.start()

        def request(body, auth=token, path="/v1/systemone", method="POST", extra=None):
            conn = http.client.HTTPConnection(*server.server_address, timeout=3)
            try:
                headers = {"Authorization": "Bearer " + auth, "Content-Type": "application/json"}
                headers.update(extra or {})
                conn.request(method, path, body, headers)
                response = conn.getresponse()
                return response.status, json.loads(response.read())
            finally:
                conn.close()

        valid = {
            "model": identity,
            "state": {"data": {"message": "Synthetic incident", "id": 9007199254740993}},
            "questions": {
                "category": {"type": "choice", "instructions": "Classify", "criteria": {"incident": "Broken", "noise": "Routine"}},
                "action": {"type": "noul", "instructions": "Act?", "criteria": {"true": "Yes", "false": "No"}},
                "severity": {"type": "score", "instructions": "Severity?", "criteria": ["Low", "High"]},
            },
        }
        try:
            status, result = request(json.dumps(valid))
            self.assertEqual(status, 200)
            self.assertEqual(result["model"], identity)
            self.assertEqual(result["usage"]["output_tokens"], 0)
            self.assertEqual(calls[0][0]["data"]["id"], 9007199254740993)
            self.assertEqual(set(result["answers"]), set(valid["questions"]))
            self.assertEqual(request(None, path="/health", method="GET")[0], 200)
            self.assertEqual(request("{}", auth="wrong")[0], 401)
            self.assertEqual(request("{}", path="/chat/completions")[0], 404)
            self.assertEqual(request("x" * (MAX_BODY + 1))[0], 413)
            self.assertEqual(request("{}", extra={"Content-Type": "text/plain"})[0], 415)
            self.assertEqual(request("{}", extra={"Transfer-Encoding": "chunked"})[0], 400)
            for raw in ["{", "[]", "{}", '{"state": 1, "state": 2}',
                        json.dumps(valid).replace('"Synthetic incident"', "NaN"),
                        json.dumps(valid).replace('"Synthetic incident"', "1e999")]:
                self.assertEqual(request(raw)[0], 400)
            for patch in [
                {"model": model_id(True)}, {"images": ["http://never-fetch.invalid"]},
                {"questions": {}},
                {"questions": {"x": {"type": "unknown", "instructions": "?"}}},
                {"questions": {"x": {"type": "noul", "instructions": ""}}},
                {"questions": {"x": {"type": "noul", "instructions": "?", "criteria": {"true": "Yes"}}}},
                {"questions": {"x": {"type": "score", "instructions": "?", "criteria": ["One"]}}},
                {"questions": {"x": {"type": "choice", "instructions": "?", "criteria": {"a.b": "A", "b": "B"}}}},
                {"questions": {"x": {"type": "choice", "instructions": "?", "criteria": {"a": 1, "b": "B"}}}},
            ]:
                value = copy.deepcopy(valid)
                value.update(patch)
                self.assertEqual(request(json.dumps(value))[0], 400)
            self.assertEqual(len(calls), 1, "invalid requests must never reach inference")
            fail = True
            status, result = request(json.dumps(valid))
            self.assertEqual(status, 500)
            self.assertNotIn("private", json.dumps(result))
        finally:
            server.shutdown()
            worker.join(timeout=3)
            server.server_close()
            self.assertFalse(worker.is_alive())


if __name__ == "__main__":
    unittest.main()
