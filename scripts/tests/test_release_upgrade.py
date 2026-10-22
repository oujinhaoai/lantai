"""Resolved config comparison; these tests do not run Docker or an upgrade."""
from copy import deepcopy
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("release_upgrade", Path(__file__).parents[1] / "release_upgrade.py")
upgrade = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(upgrade)


class RetainedConfig(unittest.TestCase):
    def setUp(self):
        self.before = {"name": "existing-project", "volumes": {
            "existing_data": {"name": "existing-project_existing_data"},
            "existing_secrets": {"name": "existing-project_existing_secrets"}},
            "networks": {"backend": {"internal": True}}, "services": {
                "prepare": {"image": "old", "volumes": [{"type": "volume", "source": "existing_data", "target": "/var/lib/lantai"}]},
                "core": {"image": "old", "environment": {"EXISTING_SETTING": "keep"},
                         "tmpfs": ["/tmp:rw,noexec,nosuid,nodev,size=64m"],
                         "volumes": [{"type": "volume", "source": "existing_data", "target": "/var/lib/lantai"},
                                     {"type": "volume", "source": "existing_secrets", "target": "/var/lib/lantai-secrets"}]},
                "gateway": {"image": "old-caddy", "ports": [{"published": "443", "target": 443}],
                            "tmpfs": ["/tmp:rw,noexec,nosuid,nodev,size=64m"],
                            "volumes": [{"type": "bind", "source": "/old-web", "target": "/srv/lantai-web",
                                         "read_only": True, "bind": {"create_host_path": False}},
                                        {"type": "bind", "source": "/old-tls", "target": "/run/tls", "read_only": True}]}}}
        self.after = deepcopy(self.before)
        for name in ("prepare", "core"):
            self.after["services"][name]["image"] = "new"
        self.after["services"]["gateway"]["image"] = "new-caddy"
        self.after["services"]["gateway"]["volumes"][0]["source"] = "/new-web"
        self.after["services"]["core"]["environment"].update(
            LANTAI_BACKUP_ROOT="/var/lib/lantai-backup", LANTAI_BACKUP_ROOT_ID="synthetic-volume")
        self.after["services"]["core"]["volumes"].append({"type": "bind", "source": "/private-backup",
            "target": "/var/lib/lantai-backup", "bind": {"create_host_path": False}})
        for name in ("core", "gateway"):
            self.after["services"][name]["tmpfs"] = ["/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m"]

    def verify(self, proof=False):
        upgrade.assert_retained_upgrade(self.before, self.after, core_image="new", caddy_image="new-caddy",
                                        web="/new-web", backup="/private-backup", root_id="synthetic-volume", no_create_verified=proof)

    def test_only_bounded_additions_preserve_existing_gui_identifiers(self):
        original = deepcopy(self.before)
        self.verify()
        self.assertEqual(self.before, original)
        self.after["services"]["gateway"]["volumes"].reverse()
        self.verify()

    def test_changed_project_volumes_tls_environment_and_permissions_rejected(self):
        mutations = [
            lambda x: x.update(name="replacement-project"),
            lambda x: x["volumes"]["existing_data"].update(name="empty-volume"),
            lambda x: x["services"]["core"]["volumes"][0].update(source="empty-volume"),
            lambda x: x["services"]["gateway"]["volumes"][1].update(source="/replacement-tls"),
            lambda x: x["services"]["core"]["environment"].update(EXISTING_SETTING="changed"),
            lambda x: x["services"]["core"]["environment"].update(LANTAI_BACKUP_ROOT_ID="wrong"),
            lambda x: x["services"]["core"]["volumes"][-1]["bind"].update(create_host_path=True),
            lambda x: x["services"]["gateway"]["volumes"][0].update(read_only=False),
            lambda x: x["services"]["gateway"]["ports"][0].update(published="8443"),
            lambda x: x["services"]["core"].update(user="0:0"),
            lambda x: x["services"]["core"].update(command=["init"]),
        ]
        expected = deepcopy(self.after)
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.after = deepcopy(expected)
                mutation(self.after)
                with self.assertRaisesRegex(RuntimeError, "protected"):
                    self.verify()

    def test_unprotected_or_missing_old_static_mount_requires_review(self):
        for invalid in ([], [{"type": "bind", "target": "/srv/lantai-web", "source": "/old-web"}]):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                self.before["services"]["gateway"]["volumes"] = invalid
                self.verify()

    def test_omitted_false_needs_real_no_create_proof_and_true_still_fails(self):
        del self.before["services"]["gateway"]["volumes"][0]["bind"]["create_host_path"]
        del self.after["services"]["gateway"]["volumes"][0]["bind"]["create_host_path"]
        del self.after["services"]["core"]["volumes"][-1]["bind"]["create_host_path"]
        with self.assertRaisesRegex(RuntimeError, "behavior proof"):
            self.verify()
        self.verify(proof=True)
        self.after["services"]["core"]["volumes"][-1]["bind"]["create_host_path"] = True
        with self.assertRaisesRegex(RuntimeError, "protected"):
            self.verify(proof=True)


class SchemaAwareUpgrade(unittest.TestCase):
    def report(self):
        return {"compatible": False, "databases": [{"database": name, "applied_version": 14 if name == "runtime" else 5,
            "latest_version": 15 if name == "runtime" else 5} for name in ("main", "ledger", "runtime", "events", "index")]}

    def test_only_reviewed_schema_transition_is_selected(self):
        report = self.report()
        self.assertEqual(upgrade.schema_upgrade_mode(report), "runtime14-to15-offline-migration")
        report["databases"][0]["latest_version"] += 1
        with self.assertRaises(RuntimeError):
            upgrade.schema_upgrade_mode(report)
        report = self.report()
        report["databases"][2]["applied_version"] = 15
        with self.assertRaises(RuntimeError):
            upgrade.schema_upgrade_mode(report)  # Equal schemas do not hide another incompatibility.
        report["compatible"] = True
        self.assertEqual(upgrade.schema_upgrade_mode(report), "same-schema")

    def test_migration_requires_recovery_and_rejects_same_volume_rollback_claim(self):
        result = {"upgrade_mode": "runtime14-to15-offline-migration", "migration_upgrade_passed": True,
                  "independent_restore_passed": True, "direct_upgrade_passed": False,
                  "rollback_passed": False, "same_volume_old_image_rollback_supported": False}
        self.assertTrue(upgrade.upgrade_evidence_passed(result))
        for key in result.keys() - {"upgrade_mode"}:
            altered = dict(result)
            altered[key] = not result[key]
            self.assertFalse(upgrade.upgrade_evidence_passed(altered), key)
        self.assertTrue(upgrade.upgrade_evidence_passed({"direct_upgrade_passed": True, "rollback_passed": True}))
        self.assertFalse(upgrade.upgrade_evidence_passed({"direct_upgrade_passed": True}))

    def test_config_template_transition_preserves_source_and_all_other_constraints(self):
        fixture = RetainedConfig()
        fixture.setUp()
        fixture.before["services"]["core"]["volumes"].append({"type": "bind", "source": "/original-config", "target": "/var/lib/lantai/config.yaml", "read_only": True})
        fixture.before["services"]["core"]["command"] = ["start", "--home", "/var/lib/lantai", "--setup-dir", "/var/lib/lantai-setup"]
        # The baseline captures its verified backup before migrating.
        fixture.before["services"]["core"]["volumes"].append(deepcopy(fixture.after["services"]["core"]["volumes"][-1]))
        fixture.before["services"]["core"]["environment"] = deepcopy(fixture.after["services"]["core"]["environment"])
        fixture.after["services"]["core"]["volumes"].append({"type": "bind", "source": "/original-config", "target": "/etc/lantai/config.template.yaml", "read_only": True})
        fixture.after["services"]["core"]["command"] = fixture.before["services"]["core"]["command"] + ["--config-template", "/etc/lantai/config.template.yaml"]
        def verify():
            upgrade.assert_retained_upgrade(fixture.before, fixture.after, core_image="new", caddy_image="new-caddy", web="/new-web", backup="/private-backup", root_id="synthetic-volume", backup_preconfigured=True, config_template_migration=True)
        verify()
        fixture.after["services"]["core"]["volumes"][-1]["source"] = "/replacement-config"
        with self.assertRaisesRegex(RuntimeError, "protected"):
            verify()
