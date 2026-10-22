"""Fail-closed comparisons for a retained-volume release upgrade.

Only the reviewed image/static/backup additions and the explicit tmpfs mode
repair may differ. No project, volume, TLS, identity or network reset is allowed.
"""
from copy import deepcopy


def _mount(service, target):
    matches = [v for v in service.get("volumes", []) if v["target"] == target]
    if len(matches) != 1:
        raise RuntimeError("expected exactly one mount: " + target)
    return matches[0]


def assert_retained_upgrade(before, after, *, core_image, caddy_image, web, backup, root_id,
                            no_create_verified=False, backup_preconfigured=False, config_template_migration=False):
    expected, actual = deepcopy(before), deepcopy(after)
    # Some Compose serializers omit false boolean fields. Only interpret that
    # representation after this engine rejected missing source directories for
    # both original and candidate overlays. Explicit true remains forbidden.
    for config in (expected, actual):
        for name, target in (("gateway", "/srv/lantai-web"), ("core", "/var/lib/lantai-backup")):
            for volume in config["services"][name].get("volumes", []):
                if volume.get("target") == target and volume.get("type") == "bind":
                    options = volume.setdefault("bind", {})
                    if "create_host_path" not in options:
                        if not no_create_verified:
                            raise RuntimeError("missing no-create behavior proof for omitted Compose field")
                        options["create_host_path"] = False
    services = expected["services"]
    for name, image in (("prepare", core_image), ("core", core_image), ("gateway", caddy_image)):
        services[name]["image"] = image
    # Current rc.4 already has a web mount. Replace only its versioned source;
    # its read-only and no-create constraints must survive.
    mount = _mount(services["gateway"], "/srv/lantai-web")
    if mount.get("type") != "bind" or mount.get("read_only") is not True or mount.get("bind", {}).get("create_host_path") is not False:
        raise RuntimeError("baseline static mount is not protected")
    mount["source"] = str(web)
    existing = [v for v in services["core"]["volumes"] if v["target"] == "/var/lib/lantai-backup"]
    backup_mount = {"type": "bind", "source": str(backup), "target": "/var/lib/lantai-backup", "bind": {"create_host_path": False}}
    if backup_preconfigured:
        if existing != [backup_mount] or any(services["core"].get("environment", {}).get(k) != v for k, v in
                {"LANTAI_BACKUP_ROOT": "/var/lib/lantai-backup", "LANTAI_BACKUP_ROOT_ID": root_id}.items()):
            raise RuntimeError("baseline backup configuration does not match verified capture")
    else:
        if existing:
            raise RuntimeError("baseline backup configuration needs explicit review")
        services["core"].setdefault("environment", {}).update(
            LANTAI_BACKUP_ROOT="/var/lib/lantai-backup", LANTAI_BACKUP_ROOT_ID=root_id)
        services["core"]["volumes"].append(backup_mount)
    if config_template_migration:
        mount = _mount(services["core"], "/var/lib/lantai/config.yaml")
        if mount.get("type") != "bind" or mount.get("read_only") is not True:
            raise RuntimeError("legacy config is not a read-only bind")
        if services["core"].get("command") != ["start", "--home", "/var/lib/lantai", "--setup-dir", "/var/lib/lantai-setup"]:
            raise RuntimeError("legacy start command needs explicit review")
        mount["target"] = "/etc/lantai/config.template.yaml"
        services["core"]["command"] += ["--config-template", "/etc/lantai/config.template.yaml"]
    for name in ("core", "gateway"):
        services[name]["tmpfs"] = [value.replace(
            "/tmp:rw,noexec,nosuid,nodev,size=64m", "/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m")
            for value in services[name].get("tmpfs", [])]
    for config in (expected, actual):
        for service in config["services"].values():
            if "volumes" in service:
                service["volumes"].sort(key=lambda v: v["target"])
    if expected != actual:
        raise RuntimeError("upgrade changed protected project/volume/TLS/environment configuration")


def schema_upgrade_mode(report):
    """Select only validated transitions; a new schema pair needs a new test."""
    pairs = {row["database"]: (row["applied_version"], row["latest_version"]) for row in report["databases"]}
    if len(report["databases"]) != 5 or set(pairs) != {"main", "ledger", "runtime", "events", "index"}:
        raise RuntimeError("expected five database schema reports")
    changed = {name: versions for name, versions in pairs.items() if versions[0] != versions[1]}
    if not changed and report.get("compatible") is True:
        return "same-schema"
    if changed == {"runtime": (14, 15)}:
        return "runtime14-to15-offline-migration"
    raise RuntimeError("unsupported schema transition needs explicit verification")


def upgrade_evidence_passed(result):
    """Neither a schema change nor an unsupported same-volume rollback can be hidden."""
    if result.get("upgrade_mode", "same-schema") == "same-schema":
        return result.get("direct_upgrade_passed") is True and result.get("rollback_passed") is True
    if result.get("upgrade_mode") == "runtime14-to15-offline-migration":
        return (result.get("migration_upgrade_passed") is True
                and result.get("independent_restore_passed") is True
                and result.get("direct_upgrade_passed") is False
                and result.get("rollback_passed") is False
                and result.get("same_volume_old_image_rollback_supported") is False)
    return False
