import ast
import http.client
import importlib.util
import json
import errno
import subprocess
from pathlib import Path
import unittest
from unittest.mock import Mock, patch


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


class UnclaimedBusinessGate(unittest.TestCase):
    def test_selector_requires_claim_and_legacy_hides_business_route(self):
        for script, expected in (("entry.js", 401), ("setup.js", 404)):
            page = f'<script src="/setup/{script}" defer></script>'.encode()
            with self.subTest(script=script):
                request = Mock(return_value=(expected, b"", None))
                smoke.assert_unclaimed_business_closed(page, request)
                request.assert_called_once_with("/api/v1/meta")
                for status in (200, 403, 500, 404 if expected == 401 else 401):
                    with self.subTest(status=status), self.assertRaises(AssertionError):
                        smoke.assert_unclaimed_business_closed(
                            page, Mock(return_value=(status, b"", None)))

    def test_unknown_setup_page_is_not_assumed_compatible(self):
        request = Mock()
        with self.assertRaisesRegex(AssertionError, "Unknown first-start page"):
            smoke.assert_unclaimed_business_closed(b"unknown page", request)
        request.assert_not_called()


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


class RestartDeadline(unittest.TestCase):
    def run_restart_sequence(self, ready_after, diagnostic_seconds, request_seconds=0):
        # Execute the real wait and restart control flow with a virtual clock.
        # Docker commands, TCP, TLS, sleeps and credentials are never used.
        tree = ast.parse(Path(smoke.__file__).read_text())
        wait_node = next(node for node in ast.walk(tree)
                         if isinstance(node, ast.FunctionDef) and node.name == "wait")
        loop = next(node for node in ast.walk(tree)
                    if isinstance(node, ast.For) and isinstance(node.target, ast.Name)
                    and node.target.id == "cycle")

        class Clock:
            now = 0.0

            def monotonic(self):
                return self.now

            def sleep(self, seconds):
                self.now += seconds

        clock = Clock()
        up_times, ready_times, meta_times = [], [], []
        self.request_timeouts = []

        def command(argv):
            if argv[-2:] == ["up", "-d"]:
                up_times.append(clock.now)

        def record(phase):
            if phase.endswith("-returned"):
                clock.now += diagnostic_seconds
            elif phase.endswith("-ready"):
                ready_times.append(clock.now - up_times[-1])

        def request(path, timeout=5):
            if path == "/setup/":
                return (404, b"", None)
            self.request_timeouts.append(timeout)
            # Deliberately allow transport completion to overrun the per-operation
            # timeout: acceptance must independently check the absolute deadline.
            clock.now += request_seconds
            elapsed = clock.now - up_times[-1]
            meta_times.append(elapsed)
            if elapsed >= ready_after:
                return (200, b"", None)
            raise ConnectionRefusedError("synthetic not ready")

        env = {"time": clock, "http": http, "request": request, "command": command,
               "record": record, "compose": ["synthetic-compose"], "port": 32444}
        module = ast.Module(body=[wait_node, loop], type_ignores=[])
        try:
            exec(compile(ast.fix_missing_locations(module), "<restart deadline>", "exec"), env)
            passed = True
        except AssertionError:
            passed = False
        return passed, ready_times, meta_times, clock.now - up_times[-1]

    def test_diagnostics_cannot_admit_service_ready_after_original_deadline(self):
        passed, ready, _, elapsed = self.run_restart_sequence(70, 20)
        self.assertFalse(passed)
        self.assertEqual(ready, [])
        self.assertEqual(elapsed, 60)

    def test_each_cycle_can_recover_within_its_original_window(self):
        passed, ready, _, _ = self.run_restart_sequence(55, 20)
        self.assertTrue(passed)
        self.assertEqual(ready, [55, 55, 55])

    def test_diagnostics_exhausting_window_cannot_start_another_wait(self):
        for diagnostic_seconds in (60, 65):
            with self.subTest(diagnostic_seconds=diagnostic_seconds):
                passed, ready, requests, elapsed = self.run_restart_sequence(0, diagnostic_seconds)
                self.assertFalse(passed)
                self.assertEqual(ready, [])
                self.assertEqual(requests, [])
                self.assertEqual(elapsed, diagnostic_seconds)

    def test_response_completing_after_deadline_is_rejected(self):
        for diagnostics, ready_after, expected_elapsed in ((57, 61, 61), (20, 61, 62.25)):
            with self.subTest(diagnostics=diagnostics):
                passed, ready, _, elapsed = self.run_restart_sequence(ready_after, diagnostics, 4)
                self.assertFalse(passed)
                self.assertEqual(ready, [])
                self.assertEqual(elapsed, expected_elapsed)

    def test_response_completing_exactly_at_deadline_is_rejected(self):
        passed, ready, _, elapsed = self.run_restart_sequence(60, 57, 3)
        self.assertFalse(passed)
        self.assertEqual(ready, [])
        self.assertEqual(elapsed, 60)

    def test_response_completing_before_deadline_passes_each_cycle(self):
        passed, ready, _, _ = self.run_restart_sequence(59, 57, 2)
        self.assertTrue(passed)
        self.assertEqual(ready, [59, 59, 59])
        self.assertEqual(self.request_timeouts, [3, 3, 3])

    def test_last_request_uses_remaining_budget(self):
        self.run_restart_sequence(61, 20, 4)
        self.assertEqual(self.request_timeouts[-1], 1.75)
        self.assertTrue(all(0 < timeout <= 5 for timeout in self.request_timeouts))

    def test_remaining_timeout_reaches_connection_and_tcp_socket(self):
        tree = ast.parse(Path(smoke.__file__).read_text())
        request_node = next(node for node in ast.walk(tree)
                            if isinstance(node, ast.FunctionDef) and node.name == "request")
        connect_node = next(node for node in ast.walk(tree)
                            if isinstance(node, ast.FunctionDef) and node.name == "connect")
        connection, context, sock = Mock(), Mock(), Mock()
        local_tls = Mock(return_value=connection)
        socket_module = Mock()
        socket_module.create_connection.return_value = sock
        env = {"LocalTLS": local_tls, "context": context, "port": 32444,
               "origin": "https://library.example:32444", "json": json,
               "socket": socket_module}
        module = ast.Module(body=[request_node, connect_node], type_ignores=[])
        exec(compile(ast.fix_missing_locations(module), "<request budget>", "exec"), env)
        env["request"]("/api/v1/meta", timeout=1.75)
        local_tls.assert_called_once_with("library.example", 32444, context=context, timeout=1.75)
        connection.close.assert_called_once()
        transport = Mock(timeout=1.75)
        env["connect"](transport)
        socket_module.create_connection.assert_called_once_with(("127.0.0.1", 32444), timeout=1.75)
        context.wrap_socket.assert_called_once_with(sock, server_hostname="library.example")

    def test_no_diagnostics_preserves_original_timeout(self):
        passed, ready, _, elapsed = self.run_restart_sequence(70, 0)
        self.assertFalse(passed)
        self.assertEqual(ready, [])
        self.assertEqual(elapsed, 60)


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
