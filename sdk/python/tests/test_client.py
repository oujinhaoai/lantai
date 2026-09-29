import hashlib
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from lantai import Client, APIError, ProtocolError

ID = '01ARZ3NDEKTSV4RRFFQ69G5FAV'


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.requests = []
        self.response = (200, {'api_version': 'v1'})
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.handle_request()

            def do_POST(self):
                self.handle_request()

            def handle_request(self):
                raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                owner.requests.append((self.command, self.path, dict(self.headers), raw))
                status, body = owner.response(self.path) if callable(owner.response) else owner.response
                raw = body if isinstance(body, bytes) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.client = Client(f'http://127.0.0.1:{self.server.server_port}', 'session-secret', allow_http=True)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def test_contract_headers_pagination(self):
        self.client.tasks(ID, after=ID, limit=10)
        _, path, headers, _ = self.requests[-1]
        self.assertIn('after='+ID, path)
        self.assertEqual(headers['Authorization'], 'Bearer session-secret')
        self.client.task_command('claim', {'task_id': ID}, key='once')
        _, _, headers, body = self.requests[-1]
        self.assertEqual(headers['Idempotency-Key'], 'once')
        self.assertEqual(json.loads(body), {'task_id': ID})
        for limit in [0, 101, True, '1']:
            with self.assertRaises(ValueError):
                self.client.tasks(ID, limit=limit)
        with self.assertRaises(ValueError):
            self.client.task_command('shell', {}, key='once')

    def test_errors_redaction_and_no_retry(self):
        self.response = (409, {'error': {'code': 'LEASE_STALE', 'message': 'session-secret', 'retryable': False,
                                      'recovery_action': 'reconcile', 'operation_id': ID}})
        with self.assertRaises(APIError) as caught:
            self.client.task_command('claim', {}, key='same-key')
        e = caught.exception
        self.assertEqual((e.code, e.retryable, e.recovery_action, e.operation_id), ('LEASE_STALE', False, 'reconcile', ID))
        self.assertNotIn('session-secret', str(e))
        self.assertEqual(len(self.requests), 1)

    def test_auth_exchange_and_version(self):
        self.response = (200, {'token': 'new-secret', 'session': {'id': ID}})
        self.assertEqual(self.client.exchange('bootstrap-secret'), {'id': ID})
        self.assertNotIn('Authorization', self.requests[-1][2])
        self.client.whoami()
        self.assertEqual(self.requests[-1][2]['Authorization'], 'Bearer new-secret')
        self.response = (200, {'api_version': 'v2'})
        with self.assertRaises(ProtocolError):
            self.client.meta()
        self.response = (302, {})
        with self.assertRaises(ProtocolError):
            self.client.meta()

    def test_origins_and_routes(self):
        for url in ['http://example.com', 'https://u:p@example.com', 'https://example.com/path']:
            with self.assertRaises(ValueError):
                Client(url, allow_http=True)
        for path in ['https://example.com/api/v1/meta', '/api/v1/../meta', '/api/v1/%2e%2e/meta', '/api/v1/shell']:
            with self.assertRaises(ValueError):
                self.client.request('GET', path)
        with self.assertRaises(ValueError):
            self.client.task_command('claim', {}, key='bad\r\nInjected: yes')
        self.assertEqual(self.requests, [])

    def test_streaming_exact_download_and_destination_safety(self):
        content = b'synthetic file\n'
        expected = {'path': 'sub/example.txt', 'size': len(content), 'sha256': hashlib.sha256(content).hexdigest()}
        def response(path):
            if '/read-grants' in path:
                return 200, dict(expected, url='/xfer/file?grant=opaque')
            if path.startswith('/xfer/'):
                return 200, content
            return 200, {'manifest': {'content': {'files': [expected]}}}
        self.response = response
        with tempfile.TemporaryDirectory() as directory:
            out = self.client.download_file(ID, ID, expected['path'], directory)
            self.assertEqual(out.read_bytes(), content)
            with self.assertRaises(FileExistsError):
                self.client.download_file(ID, ID, expected['path'], directory)
            self.assertEqual(out.read_bytes(), content)
            out.unlink()
            expected['sha256'] = '0'*64
            with self.assertRaises(ProtocolError):
                self.client.download_file(ID, ID, expected['path'], directory)
            self.assertFalse(out.exists())
            self.assertEqual(list(out.parent.glob('.lantai-download-*')), [])
            for path in ['../escape', 'sub//example.txt', '/absolute', 'x\\y']:
                with self.assertRaises(ValueError):
                    self.client.download_file(ID, ID, path, directory)


if __name__ == '__main__':
    unittest.main()
