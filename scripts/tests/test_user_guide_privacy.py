"""Synthetic publication checks; never embed a maintainer's private identifiers."""
import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

ROOT = Path(__file__).parents[2]
SPEC = importlib.util.spec_from_file_location("public_help_privacy", ROOT / "docs/user-guide/privacy.py")
privacy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(privacy)


class PublicHelpPrivacy(unittest.TestCase):
    def test_portable_examples_system_paths_and_official_links(self):
        examples = (
            "C:/Users/Example/lantai-work", "/path/to/lantai-work", "~/lantai-data",
            "$HOME/lantai-data", "/usr/local/bin", "/var/lib/lantai", "/etc/lantai",
            "library.example", "https://library.example:8443/setup/",
            "https://learn.chatgpt.com/docs/extend/mcp", "https://github.com/example/project",
            "https://localhost:8443", "http://127.0.0.1:9090", "http://127.2.3.4:8000", "http://[::1]:8000",
            'api_key="YOUR_API_KEY"', "password=placeholder", "Authorization: Bearer REDACTED",
        )
        for index, text in enumerate(examples):
            with self.subTest(example=index): self.assertEqual(privacy.privacy_issues(text), ())

    def test_machine_paths_and_example_boundary(self):
        examples = (
            "/Users/synthetic/work", r"C:\Users\Synthetic\work", r"C:\\Users\\Synthetic\\work",
            "//Users/synthetic/work", "/home/synthetic/work",
            "/Volumes/SyntheticDisk/work", "/mnt/synthetic/work", "/media/synthetic/work",
            "C:/Users/Example/lantai-work-private", "C:/Users/Example/lantai-work/private",
            "%2FUsers%2Fsynthetic%2Fwork", "&#47;Volumes&#47;SyntheticDisk&#47;work",
        )
        for index, text in enumerate(examples):
            with self.subTest(example=index): self.assertIn("machine-path", privacy.privacy_issues(text))

    def test_private_addresses_and_internal_domains(self):
        for index, text in enumerate(("10.23.45.67", "172.16.0.1", "172.31.255.254", "192.168.45.67")):
            with self.subTest(address=index): self.assertIn("rfc1918-address", privacy.privacy_issues(text))
        for index, text in enumerate(("service.internal", "node.local", "service.lan", "service.corp", "service.home.arpa")):
            with self.subTest(domain=index): self.assertIn("internal-domain", privacy.privacy_issues(text))
        self.assertIn("non-loopback-endpoint", privacy.privacy_issues("http://[fd00::1234]:8000"))

    def test_unreviewed_deployment_hosts_not_maintainer_denylist(self):
        for index, text in enumerate(("https://archive.unreviewed/setup", "https://example.com.unreviewed/", "synthetic-deployment.net")):
            with self.subTest(domain=index): self.assertIn("unreviewed-host", privacy.privacy_issues(text))
        for text in ("https://archive.example", "https://archive.example.org", "https://host.invalid"):
            self.assertEqual(privacy.privacy_issues(text), ())

    def test_credentials_and_encoding(self):
        for prefix in ("ltk_", "lts_"):
            self.assertIn("lantai-token", privacy.privacy_issues(prefix + "x" * 24))
        samples = (
            'password="' + "synthetic-value-123" + '"', "Authorization: Bearer " + "z" * 24,
            "ghp_" + "x" * 36, "sk-proj-" + "x" * 32, "-----BEGIN PRIVATE KEY-----",
            "api_key&#61;&quot;" + "x" * 24 + "&quot;", "api_key%3D" + "y" * 24,
            "otpauth://totp/Example?secret=" + "A" * 16,
        )
        for index, text in enumerate(samples):
            with self.subTest(credential=index): self.assertIn("credential", privacy.privacy_issues(text))

    def test_generated_markup_keeps_the_same_host_boundary(self):
        accepted = '<a href="https://learn.chatgpt.com/docs/extend/mcp">docs</a>'
        rejected = '<a href="https://archive&#46;unreviewed/setup">instance</a>'
        self.assertEqual(privacy.privacy_issues(accepted), ())
        self.assertIn("unreviewed-host", privacy.privacy_issues(rejected))
        self.assertIn("machine-path", privacy.privacy_issues('<code>%2FUsers%2Fsynthetic%2Fwork</code>'))

    def test_official_host_does_not_allow_url_credentials(self):
        samples = (
            "https://synthetic:abcdefgh123@github.com/",
            '<a href="https://synthetic:abcdefgh123@github.com/">docs</a>',
            "https://synthetic%3Aabcdefgh123%40github.com/",
            '<a href="https://synthetic&#58;abcdefgh123&#64;github.com/">docs</a>',
            "https://synthetic@github.com/",
        )
        for index, text in enumerate(samples):
            with self.subTest(sample=index): self.assertIn("credential", privacy.privacy_issues(text))

    def test_diagnostics_do_not_return_matches(self):
        value = "/Users/synthetic/work"
        result = privacy.privacy_issues(value)
        self.assertEqual(result, ("machine-path",))
        self.assertNotIn(value, repr(result))

    def test_current_markdown_and_generated_help(self):
        result = subprocess.run([sys.executable, "docs/user-guide/check.py"], cwd=ROOT,
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("generated/CSP/privacy/shell checks", result.stdout)


if __name__ == "__main__":
    unittest.main()
