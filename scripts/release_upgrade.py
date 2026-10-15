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
                            no_create_verified=False):
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
    if any(v["target"] == "/var/lib/lantai-backup" for v in services["core"]["volumes"]):
        raise RuntimeError("baseline backup configuration needs explicit review")
    services["core"].setdefault("environment", {}).update(
        LANTAI_BACKUP_ROOT="/var/lib/lantai-backup", LANTAI_BACKUP_ROOT_ID=root_id)
    services["core"]["volumes"].append({"type": "bind", "source": str(backup),
        "target": "/var/lib/lantai-backup", "bind": {"create_host_path": False}})
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
