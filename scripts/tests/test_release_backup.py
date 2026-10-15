"""Host fixture and command boundaries; real Linux behavior is tested separately."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("release_backup", Path(__file__).parents[1] / "release_backup.py")
backup = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(backup)


class BackupFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.root = self.base / "backup"
        self.root.mkdir(mode=0o700)
        self.marker = self.root / backup.MARKER
        self.marker.write_text("synthetic-volume\n")
        self.marker.chmod(0o600)

    def command(self):
        return backup.prepare_backup_command(self.root, "reviewed-caddy")

    def test_only_required_capabilities_and_one_private_mount(self):
        command = self.command()
        caps = [command[i + 1] for i, item in enumerate(command) if item == "--cap-add"]
        self.assertEqual(caps, ["CHOWN", "DAC_READ_SEARCH"])
        self.assertEqual(command[command.index("--cap-drop") + 1], "ALL")
        self.assertEqual(command[command.index("--network") + 1], "none")
        self.assertIn("--read-only", command)
        self.assertEqual(command.count("--mount"), 1)
        self.assertEqual(command[command.index("--mount") + 1], f"type=bind,source={self.root},target=/backup")
        self.assertNotIn("--privileged", command)
        self.assertIn("chown -h 65532:65532 /backup/.lantai-backup-root /backup", backup.PREPARE_SCRIPT)
        self.assertEqual(self.root.stat().st_uid, os.geteuid())
        self.assertEqual(self.marker.read_text(), "synthetic-volume\n")

    def test_non_private_modes_are_rejected_before_container_start(self):
        for mode in (0o777, 0o750, 0o755):
            with self.subTest(mode=mode):
                self.root.chmod(mode)
                with self.assertRaises(RuntimeError):
                    self.command()
        self.root.chmod(0o700)
        self.marker.chmod(0o644)
        with self.assertRaises(RuntimeError):
            self.command()

    def test_links_and_extra_entries_are_rejected(self):
        outside = self.base / "outside"
        self.marker.rename(outside)
        self.marker.symlink_to(outside)
        with self.assertRaises(RuntimeError):
            self.command()
        self.marker.unlink()
        os.link(outside, self.marker)
        with self.assertRaises(RuntimeError):
            self.command()
        outside.unlink()
        extra = self.root / "extra"
        extra.write_text("synthetic")
        with self.assertRaises(RuntimeError):
            self.command()

    def test_symlink_root_is_rejected(self):
        alias = self.base / "alias"
        alias.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(RuntimeError):
            backup.prepare_backup_command(alias, "reviewed-caddy")
