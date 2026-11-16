import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('native', Path(__file__).resolve().parents[1] / 'test-native-macos.py')
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)


class NativeSmokeTests(unittest.TestCase):
    def test_missing_or_changed_asset_fails(self):
        for response in ((404, b''), (200, b'wrong')):
            with patch.object(native, 'fetch', return_value=response):
                with self.assertRaises(RuntimeError):
                    native.require_resource(None, 'https://localhost/asset.js', b'expected')
        with patch.object(native, 'fetch', return_value=(200, b'expected')):
            native.require_resource(None, 'https://localhost/asset.js', b'expected')

    def test_all_listeners_must_be_loopback_and_expected(self):
        native.check_listeners('TCP 127.0.0.1:1234 (LISTEN)', [1234])
        for output in ('', 'TCP *:1234 (LISTEN)', 'TCP [::]:1234 (LISTEN)',
                       'TCP 127.0.0.1:1234 (LISTEN)\nTCP 127.0.0.1:1235 (LISTEN)'):
            with self.assertRaises(RuntimeError):
                native.check_listeners(output, [1234])

    def test_proxy_disabled_before_http_and_https(self):
        with patch.object(native.urllib.request, 'build_opener'), patch.object(native.urllib.request, 'ProxyHandler') as handler:
            native.opener()
            handler.assert_called_once_with({})

    def test_readiness_stops_when_child_exits(self):
        from unittest.mock import Mock
        process = Mock()
        process.poll.return_value = 1
        with self.assertRaises(RuntimeError):
            native.wait_ready(None, 'http://localhost', process)

    def test_cleanup_attempts_every_child_on_failure(self):
        first, second = object(), object()
        with patch.object(native, 'stop', side_effect=[OSError('failure'), None]) as stop:
            with self.assertRaises(RuntimeError):
                native.stop_all([first, second])
            self.assertEqual([call.args[0] for call in stop.call_args_list], [second, first])
