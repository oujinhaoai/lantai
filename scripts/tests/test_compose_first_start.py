import importlib.util
import json
import errno
import subprocess
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


class LifecycleDiagnostics(unittest.TestCase):
    def test_state_allowlist_excludes_environment_logs_labels_and_error_text(self):
        private = "synthetic-secret-do-not-serialize"
        info = {"Id": "a" * 64, "Image": "sha256:" + "b" * 64, "RestartCount": 0,
                "Config": {"Env": [private], "Labels": {"secret": private}},
                "Mounts": [{"Source": private}], "LogPath": private,
                "State": {"Status": "exited", "Running": False, "ExitCode": 0,
                          "Error": private, "Health": {"Log": [private]}},
                "HostConfig": {"PortBindings": {"443/tcp": [{"HostIp": "", "HostPort": "32444"}]},
                               "Binds": [private]},
                "NetworkSettings": {"Ports": {}, "Networks": {
                    "synthetic_edge": {"IPAddress": "172.19.0.2", "Gateway": "172.19.0.1", "Aliases": [private]},
                    "synthetic_backend": {"IPAddress": "172.20.0.2", "Gateway": ""}}}}
        report = smoke.public_container_state(info)
        self.assertNotIn(private, json.dumps(report))
        self.assertEqual(report["state"]["ExitCode"], 0)
        self.assertEqual(report["configured_https_bindings"][0]["HostPort"], "32444")
        self.assertIsNone(report["actual_https_bindings"])
        self.assertEqual(report["networks"]["backend"]["Gateway"], "")

    def test_stopped_container_null_port_maps_are_reported_without_error(self):
        info = {"Id": "a" * 64, "Image": "sha256:" + "b" * 64,
                "State": {"Status": "exited", "Running": False, "ExitCode": 0},
                "HostConfig": {"PortBindings": None}, "NetworkSettings": {"Ports": None}}
        with patch.object(smoke, "tcp_probe", return_value={"connected": False}), patch.object(
                smoke.subprocess, "run", side_effect=lambda argv, **kwargs: subprocess.CompletedProcess(
                    argv, 0, json.dumps([info]) if argv[:2] == ["docker", "inspect"] else "a" * 64)):
            report = smoke.lifecycle_snapshot(["docker", "compose"], 32444)
        self.assertEqual(report["request_endpoint"]["port"], 32444)
        for service in ("prepare", "core", "gateway"):
            self.assertEqual(report["services"][service]["state"]["Status"], "exited")
            self.assertIsNone(report["services"][service]["actual_https_bindings"])

    def test_snapshot_distinguishes_missing_host_mapping_from_healthy_container(self):
        info = {"Id": "a" * 64, "Image": "sha256:" + "b" * 64,
                "State": {"Status": "running", "Running": True, "ExitCode": 0},
                "HostConfig": {"PortBindings": {"443/tcp": [{"HostIp": "", "HostPort": "32444"}]}},
                "NetworkSettings": {"Ports": {"443/tcp": None}, "Networks": {
                    "synthetic_edge": {"IPAddress": "172.19.0.2", "Gateway": "172.19.0.1"}}}}
        def run(argv, **kwargs):
            if argv[:2] == ["docker", "inspect"]:
                return subprocess.CompletedProcess(argv, 0, json.dumps([info]))
            return subprocess.CompletedProcess(argv, 0, "a" * 64)
        with patch.object(smoke, "tcp_probe", side_effect=lambda address, port: {
                "address": address, "port": port, "connected": address != "127.0.0.1"}), patch.object(
                smoke.subprocess, "run", side_effect=run):
            report = smoke.lifecycle_snapshot(["docker", "compose"], 32444)
        self.assertFalse(report["host_https_tcp"]["connected"])
        gateway = report["services"]["gateway"]
        self.assertTrue(gateway["state"]["Running"])
        self.assertTrue(gateway["container_tcp"][0]["connected"])
        self.assertEqual(gateway["gateway_to_core_meta_exit_code"], 0)
        self.assertEqual(gateway["configured_https_bindings"][0]["HostPort"], "32444")
        self.assertIsNone(gateway["actual_https_bindings"])

    def test_diagnostic_failure_never_echoes_subprocess_secrets(self):
        with patch.object(smoke, "tcp_probe", return_value={"connected": False}), patch.object(
                smoke.subprocess, "run", side_effect=subprocess.CalledProcessError(
                    1, ["docker"], output="synthetic-password", stderr="synthetic-totp")):
            report = smoke.lifecycle_snapshot(["docker", "compose"], 32444)
        self.assertNotIn("synthetic-password", json.dumps(report))
        self.assertNotIn("synthetic-totp", json.dumps(report))
        self.assertEqual(report["services"]["gateway"], {"diagnostic_error_type": "CalledProcessError"})

    def test_refusal_records_exact_numeric_endpoint_and_errno(self):
        with patch.object(smoke.socket, "create_connection", side_effect=ConnectionRefusedError(
                errno.ECONNREFUSED, "untrusted diagnostic message")):
            report = smoke.tcp_probe("127.0.0.1", 32444)
        self.assertEqual(report, {"address": "127.0.0.1", "port": 32444, "connected": False,
                                 "error_type": "ConnectionRefusedError", "errno": errno.ECONNREFUSED})


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
