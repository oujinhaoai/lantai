import ssl
from pathlib import Path
import tempfile
import unittest

from lantai import Client


class ClientCATests(unittest.TestCase):
    def test_selected_bundle_scopes_trust_and_keeps_hostname_verification(self):
        roots = ssl.create_default_context().get_ca_certs(binary_form=True)
        if not roots:
            self.skipTest("no system CA available to form public test bundle")
        with tempfile.TemporaryDirectory() as directory:
            ca = Path(directory) / "ca.pem"
            ca.write_text(ssl.DER_cert_to_PEM_cert(roots[0]))
            client = Client("https://127.0.0.1", ca_file=str(ca))
            self.assertEqual(len(client._tls.get_ca_certs()), 1)
            self.assertTrue(client._tls.check_hostname)
            self.assertEqual(client._tls.verify_mode, ssl.CERT_REQUIRED)
            self.assertGreater(len(ssl.create_default_context().get_ca_certs()), 1)

    def test_bad_bundle_has_no_system_trust_fallback_and_http_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            ca = Path(directory) / "invalid.pem"
            ca.write_text("not a certificate")
            with self.assertRaises(ssl.SSLError):
                Client("https://127.0.0.1", ca_file=str(ca))
            with self.assertRaises(ValueError):
                Client("http://127.0.0.1", allow_http=True, ca_file=str(ca))
