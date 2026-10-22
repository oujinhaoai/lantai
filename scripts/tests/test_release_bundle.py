"""Real archive/member/hash guards; no simulated Docker acceptance claims."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("release_bundle", Path(__file__).parents[1] / "release_bundle.py")
bundle = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bundle)


class StaticArchives(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dist = self.root / "dist"
        (self.dist / "assets").mkdir(parents=True)
        (self.dist / "help").mkdir()
        (self.dist / "index.html").write_text('<script src="/assets/index.js"></script><link href="/assets/index.css">')
        for name in ("index.js", "index.css", "THIRD_PARTY_NOTICES.txt"):
            (self.dist / "assets" / name).write_text("public production output")
        for page in bundle.HELP_PAGES:
            (self.dist / "help" / (page + ".html")).write_text('<link href="help.css"><a href="/">return</a>')
        for name in ("help.css", "help.js", "search-data.js", "icon.svg"):
            (self.dist / "help" / name).write_text("public static output")

    def test_actual_tar_roundtrip_fixed_metadata_and_content(self):
        files = bundle.validate_static(self.dist)
        first, second = self.root / "first.tar.gz", self.root / "second.tar.gz"
        for archive in (first, second):
            bundle.write_tree_archive(self.dist, archive, "web", 12345, files)
        self.assertEqual(first.read_bytes(), second.read_bytes())
        extracted = self.root / "extracted"
        bundle.extract_verified_archive(first, extracted, "web", files)
        self.assertEqual(bundle.validate_static(extracted), files)
        changed = json.loads(json.dumps(files))
        changed[0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "content differs"):
            bundle.extract_verified_archive(first, self.root / "tampered", "web", changed)

    def test_runtime_credentials_links_and_missing_help_are_rejected(self):
        for name in (".env", "assets/private.pem", "help/runtime.db", "assets/index.js.map"):
            with self.subTest(name=name):
                path = self.dist / name
                path.write_text("fictional private negative fixture")
                with self.assertRaisesRegex(RuntimeError, "unexpected"):
                    bundle.validate_static(self.dist)
                path.unlink()
        link = self.dist / "assets/link.js"
        link.symlink_to(self.dist / "assets/index.js")
        with self.assertRaisesRegex(RuntimeError, "link or special"):
            bundle.validate_static(self.dist)
        link.unlink()
        (self.dist / "help/upgrade.html").unlink()
        with self.assertRaisesRegex(RuntimeError, "required help"):
            bundle.validate_static(self.dist)

    def test_html_csp_external_resources_and_path_escape_are_rejected(self):
        for html in ('<script>alert(1)</script>', '<div style="color:red">x</div>',
                     '<script src="https://example.invalid/code.js"></script>',
                     '<a href="javascript:alert(1)">bad</a>', '<a href="../../private.txt">bad</a>',
                     '<script src="/assets/missing.js"></script>'):
            with self.subTest(html=html):
                (self.dist / "index.html").write_text(html)
                with self.assertRaises(RuntimeError):
                    bundle.validate_static(self.dist)

    def test_manifest_extraction_cannot_escape_or_duplicate(self):
        for paths in (["/absolute"], ["../outside"], ["same", "same"]):
            with self.subTest(paths=paths), self.assertRaises(RuntimeError):
                bundle.extract_verified_archive(self.root / "unused.tar.gz", self.root / "destination", "web",
                    [{"path": p, "size_bytes": 1, "sha256": "0" * 64} for p in paths])

    def test_existing_isolated_dist_refused_before_tool_execution(self):
        source = self.root / "source"
        (source / "web/dist").mkdir(parents=True)
        def no_run(*args, **kwargs):
            self.fail("stale dist must fail before any build command")
        with self.assertRaisesRegex(RuntimeError, "existing dist"):
            bundle.build_web_bundle(source, self.root, "v0.1.0-rc.4", "a" * 40, 1, no_run)

    def test_remote_docker_context_is_rejected_before_mutation(self):
        calls = []
        def inspect_context(argv):
            calls.append(argv)
            return "ssh://synthetic.invalid"
        with patch.dict(bundle.os.environ, {}, clear=True), self.assertRaisesRegex(RuntimeError, "local Linux Docker"):
            bundle.require_local_docker(inspect_context)
        self.assertEqual(calls, [["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"]])

    def test_private_key_bytes_in_allowed_file_are_rejected(self):
        (self.dist / "assets/index.js").write_text('const fictional = "-----BEGIN PRIVATE KEY-----";')
        with self.assertRaisesRegex(RuntimeError, "private-key material"):
            bundle.validate_static(self.dist)


class DeploymentArchive(unittest.TestCase):
    def test_actual_backup_overlay_and_instructions_are_hashed_and_shipped(self):
        source = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            work, deliver = root / "work", root / "deliver"
            work.mkdir()
            deliver.mkdir()
            result = bundle.build_deployment_bundle(source, work, deliver, "v0.1.0-rc.5",
                                                     "a" * 40, 12345, "core-test", "caddy-test")
            names = {entry["path"] for entry in result["files"]}
            self.assertTrue({"compose.backup.yaml", "online-backup.md", "backup-management-api.md", "recovery-package.md", "recovery-control-api.md", "release-upgrade.md"} <= names)
            self.assertFalse(result["backup_mount"]["create_host_path"])
            extracted = root / "extracted"
            bundle.extract_verified_archive(deliver / result["archive"], extracted, "deploy", result["files"])
            self.assertEqual((extracted / "compose.backup.yaml").read_bytes(), (source / "deploy/compose.backup.yaml").read_bytes())
            self.assertIn("# LANTAI_BACKUP_DIR=", (extracted / "env.example").read_text())
            self.assertIn("(online-backup.md)", (extracted / "release-upgrade.md").read_text())
            self.assertIn("blob/" + "a" * 40, (extracted / "online-backup.md").read_text())
            self.assertIn("(backup-management-api.md)", (extracted / "online-backup.md").read_text())
            self.assertIn("blob/" + "a" * 40 + "/api/openapi.yaml", (extracted / "backup-management-api.md").read_text())
            with (extracted / "compose.backup.yaml").open("a") as stream:
                stream.write("tampered")
            self.assertNotEqual(bundle.inventory(extracted), result["files"])
