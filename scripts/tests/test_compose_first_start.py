import importlib.util
import json
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "compose_smoke", Path(__file__).resolve().parents[1] / "test-compose-first-start.py")
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class StartupDiagnostics(unittest.TestCase):
    def test_redacts_plain_escaped_and_prefixed_key_and_claim(self):
        key = "-----BEGIN PRIVATE KEY-----\nsynthetic-short-key-fragment\n-----END PRIVATE KEY-----\n"
        claim = "SyntheticClaim_" + "x" * 30
        for log in (key, json.dumps(key), "gateway | " + key.replace("\n", "\ngateway | "), claim):
            with self.subTest(log=log):
                redacted = smoke.redact_startup_log(log, key)
                self.assertNotIn("synthetic-short-key-fragment", redacted)
                self.assertNotIn(claim, redacted)

    def test_keeps_nonsecret_startup_reason(self):
        self.assertIn("permission denied", smoke.redact_startup_log(
            "core | permission denied", "synthetic unused key"))


if __name__ == "__main__":
    unittest.main()
