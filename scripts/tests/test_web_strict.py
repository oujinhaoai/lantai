import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    'web_strict', Path(__file__).resolve().parents[1] / 'test-web-strict.py')
strict = importlib.util.module_from_spec(spec)
spec.loader.exec_module(strict)


class PrivateTrustBoundary(unittest.TestCase):
    def test_host_namespace_is_rejected_before_any_mount(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'child.json'
            config.write_text(json.dumps({'uid': 1000, 'host_namespace': 'mnt:[same]'}))
            with patch.object(strict.os, 'geteuid', return_value=0), patch.object(
                    strict.os, 'readlink', return_value='mnt:[same]'), patch.object(
                    strict.subprocess, 'run') as run:
                with self.assertRaisesRegex(RuntimeError, 'host mount namespace'):
                    strict.child(config)
                run.assert_not_called()

    def test_test_browser_must_never_run_as_root(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / 'child.json'
            config.write_text(json.dumps({'uid': 0}))
            with patch.object(strict.os, 'geteuid', return_value=0), patch.object(
                    strict.subprocess, 'run') as run:
                with self.assertRaisesRegex(RuntimeError, 'non-root test user'):
                    strict.child(config)
                run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
