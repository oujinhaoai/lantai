"""The restored target must not shadow its new, local credential configuration."""
import importlib.util
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).parents[1]))
SPEC = importlib.util.spec_from_file_location("release_migration", Path(__file__).parents[1] / "release_migration.py")
migration = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(migration)


class RestoredConfig(unittest.TestCase):
    def test_only_legacy_host_config_bind_is_removed(self):
        source = """services:
  core:
    volumes:
      - original_data:/var/lib/lantai
      - original_keys:/var/lib/lantai-secrets
      - ${LANTAI_CONFIG_FILE:?set the reviewed config.container.yaml}:/var/lib/lantai/config.yaml:ro
"""
        result = migration.restored_compose(source)
        self.assertIn("original_data:/var/lib/lantai", result)
        self.assertIn("original_keys:/var/lib/lantai-secrets", result)
        self.assertNotIn("config.yaml:ro", result)

    def test_unknown_or_duplicate_config_binding_is_rejected(self):
        line = "      - ${LANTAI_CONFIG_FILE:?set reviewed file}:/var/lib/lantai/config.yaml:ro\n"
        for text in ("", line + line, "      - /unknown:/var/lib/lantai/config.yaml:ro\n"):
            with self.subTest(text=text), self.assertRaises(RuntimeError):
                migration.restored_compose(text)
