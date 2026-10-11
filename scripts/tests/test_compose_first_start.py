import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch


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


class ReleaseImageSelection(unittest.TestCase):
    def test_supplied_images_are_only_inspected_without_build_or_pull(self):
        with patch.object(smoke, "command", side_effect=["sha256:core", "sha256:caddy"]) as command:
            self.assertEqual(smoke.select_images("lantai:v0.1.0-rc.2", "lantai-caddy:2.11.4-id"),
                             ("lantai:v0.1.0-rc.2", "sha256:core", "lantai-caddy:2.11.4-id", "sha256:caddy"))
        self.assertTrue(all(call.args[0][:3] == ["docker", "image", "inspect"] for call in command.call_args_list))

    def test_missing_supplied_image_does_not_fall_back_to_build(self):
        with patch.object(smoke, "command", side_effect=RuntimeError("missing local image")) as command:
            with self.assertRaisesRegex(RuntimeError, "missing local image"):
                smoke.select_images("lantai:missing", None)
        command.assert_called_once()

    def test_image_reference_cannot_inject_compose_environment_lines(self):
        with patch.object(smoke, "command") as command:
            for value in ("lantai:tag\nLANTAI_DATA_DIR=/real", "--help", "image # comment"):
                with self.subTest(value=value), self.assertRaises(ValueError):
                    smoke.select_images(value, None)
        command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
