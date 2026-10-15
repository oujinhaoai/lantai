"""Command guard regressions; real Docker behavior is required by delivery CI."""
import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parents[1]))
SPEC = importlib.util.spec_from_file_location("delivery_bind_guard", Path(__file__).parents[1] / "test-release-delivery.py")
delivery = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(delivery)


class MissingBindGuard(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.instance = delivery.Instance.__new__(delivery.Instance)
        self.instance.root = Path(self.temp.name)
        self.instance.env = self.instance.root / "compose.env"
        self.original = "LANTAI_WEB_DIR=/original/web\nLANTAI_BACKUP_DIR=/original/backup\nKEEP=literal$value\n"
        self.instance.env.write_text(self.original)
        self.instance.env.chmod(0o600)
        self.instance.compose = ["docker", "compose", "--env-file", str(self.instance.env)]
        self.missing = self.instance.root / "missing-gateway-lantai_web_dir"

    def test_missing_bind_error_is_verified_and_environment_is_restored(self):
        def checked(argv, **kwargs):
            self.assertEqual(argv[-4:], ["up", "-d", "--no-deps", "gateway"])
            self.assertIn("LANTAI_WEB_DIR=" + str(self.missing), self.instance.env.read_text())
            return SimpleNamespace(returncode=1, stderr="bind source path does not exist: " + str(self.missing))
        with patch.object(subprocess, "run", side_effect=checked):
            self.assertTrue(self.instance.reject_missing_bind("gateway", "LANTAI_WEB_DIR"))
        self.assertEqual(self.instance.env.read_text(), self.original)
        self.assertEqual(self.instance.env.stat().st_mode & 0o777, 0o600)

    def test_unconfigured_variable_cannot_fake_a_missing_bind_result(self):
        with patch.object(subprocess, "run") as run, self.assertRaisesRegex(RuntimeError, "not configured"):
            self.instance.reject_missing_bind("core", "UNCONFIGURED")
        run.assert_not_called()

    def test_wrong_failure_or_created_directory_is_not_a_pass(self):
        for create in (False, True):
            def command(argv, **kwargs):
                if create:
                    self.missing.mkdir()
                return SimpleNamespace(returncode=1, stderr=("bind source path does not exist: " + str(self.missing))
                                       if create else "required variable is missing")
            with self.subTest(create=create), patch.object(subprocess, "run", side_effect=command), self.assertRaises(AssertionError):
                self.instance.reject_missing_bind("gateway", "LANTAI_WEB_DIR")
            self.assertEqual(self.instance.env.read_text(), self.original)

    def test_command_exception_still_restores_environment(self):
        with patch.object(subprocess, "run", side_effect=TimeoutError), self.assertRaises(TimeoutError):
            self.instance.reject_missing_bind("gateway", "LANTAI_WEB_DIR")
        self.assertEqual(self.instance.env.read_text(), self.original)
