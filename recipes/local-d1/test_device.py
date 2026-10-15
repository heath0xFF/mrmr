"""Pin device selection without importing Torch or allocating GPU memory."""
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

from serve import load_decider, model_id, private_host


class DeviceTest(unittest.TestCase):
    def test_identity_and_listen_boundary(self):
        self.assertTrue(model_id().endswith(":cpu-fp32"))
        self.assertTrue(model_id(True).endswith(":cpu-int8-channel"))
        self.assertTrue(model_id(device="cuda").endswith(":cuda-bf16"))
        for device, int8 in [("cuda", True), ("auto", False)]:
            with self.assertRaises(ValueError):
                model_id(int8, device)
        for host in ["127.0.0.1", "100.99.177.33"]:
            self.assertEqual(private_host(host), host)
        for host in ["0.0.0.0", "8.8.8.8", "192.168.1.2", "100.128.0.1", "::1", "localhost"]:
            with self.assertRaises(ValueError):
                private_host(host)

    def test_cuda_loader_and_token_bound(self):
        torch, auto = MagicMock(), MagicMock()
        model = auto.from_pretrained.return_value.eval.return_value.to.return_value
        model.engine.tokens.return_value = 12
        prompt = SimpleNamespace(as_question=lambda q: q)
        # The loader owns device/dtype selection; native model math is tested on hardware.
        with patch.dict(sys.modules, {"torch": torch, "transformers": SimpleNamespace(AutoModel=auto)}), \
                patch("serve.importlib.import_module", return_value=prompt):
            torch.cuda.is_available.return_value = True
            decide = load_decider(Path("/synthetic-checkpoint"), 4, device="cuda")
            self.assertEqual(auto.from_pretrained.call_args.kwargs["dtype"], torch.bfloat16)
            self.assertTrue(auto.from_pretrained.call_args.kwargs["local_files_only"])
            auto.from_pretrained.return_value.eval.return_value.to.assert_called_once_with("cuda")
            decide("synthetic", {"q": {"type": "noul", "instructions": "Relevant?"}})
            model.system_one.assert_called_once()
            model.engine.tokens.return_value = 1025
            with self.assertRaises(ValueError):
                decide("synthetic", {"q": {}})
            self.assertEqual(model.system_one.call_count, 1)
            auto.reset_mock()
            torch.cuda.is_available.return_value = False
            with self.assertRaises(RuntimeError):
                load_decider(Path("/synthetic-checkpoint"), 4, device="cuda")
            auto.from_pretrained.assert_not_called()


if __name__ == "__main__":
    unittest.main()
