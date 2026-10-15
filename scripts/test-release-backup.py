#!/usr/bin/env python3
"""Real local Linux container checks for the release backup fixture handover."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from release_backup import MARKER, prepare_backup_command
from release_bundle import require_local_docker


def run_checks(work, image, engine=("docker",)):
    checks = []

    def run(argv, success=True):
        p = subprocess.run(argv, capture_output=True, text=True, timeout=60)
        if success and p.returncode:
            raise RuntimeError("backup preparation container failed: " + p.stderr[-2000:])
        return p

    def check(name, condition):
        if not condition:
            raise AssertionError(name)
        checks.append({"name": name, "passed": True})
        print("PASS: " + name, flush=True)

    def fixture(name):
        root = work / name
        root.mkdir(mode=0o700)
        (root / MARKER).write_text("synthetic-release-volume\n")
        (root / MARKER).chmod(0o600)
        return root

    def consumer(root, user, script):
        return [*engine, "run", "--rm", "--network", "none", "--read-only", "--user", user,
                "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--mount",
                f"type=bind,source={root},target=/backup", "--entrypoint", "/bin/sh", image, "-ec", script]

    old = fixture("legacy")
    legacy = [*engine, "run", "--rm", "--network", "none", "--read-only", "--user", "0:0",
              "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--mount",
              f"type=bind,source={old},target=/backup", "--entrypoint", "/bin/sh", image,
              "-ec", "chown 65532:65532 /backup /backup/.lantai-backup-root"]
    denied = run(legacy, success=False)
    check("legacy CHOWN/FOWNER cannot traverse private directory after handover",
          denied.returncode != 0 and "Permission denied" in denied.stderr)

    if os.geteuid() != 0:
        reverse = fixture("reverse-only")
        command = list(legacy)
        command[command.index("--mount") + 1] = f"type=bind,source={reverse},target=/backup"
        command[-1] = "chown 65532:65532 /backup/.lantai-backup-root /backup"
        denied = run(command, success=False)
        check("order alone cannot traverse a non-root host-owned fixture",
              denied.returncode != 0 and "Permission denied" in denied.stderr)

    root = fixture("fixed")
    prepared = prepare_backup_command(root, image, engine)
    capabilities = list(prepared)
    capabilities[capabilities.index("-ec") + 1] = "grep '^CapEff:' /proc/self/status"
    effective = int(run(capabilities).stdout.split(":", 1)[1].strip(), 16)
    check("helper has only CHOWN and DAC_READ_SEARCH effective capabilities", effective == 5)
    run(prepared)
    run(consumer(root, "65532:65532", '''
test "$(id -u)" = 65532
test "$(stat -c '%a:%u:%g' /backup)" = '700:65532:65532'
test "$(stat -c '%a:%u:%g' /backup/.lantai-backup-root)" = '600:65532:65532'
test "$(cat /backup/.lantai-backup-root)" = synthetic-release-volume
printf test > /backup/consumer-probe
test "$(cat /backup/consumer-probe)" = test
rm /backup/consumer-probe
'''))
    check("non-root service reads marker and writes after private handover", True)
    denied = run(consumer(root, "65533:65533", "cat /backup/.lantai-backup-root"), success=False)
    check("different unprivileged user cannot read the marker", denied.returncode != 0)

    for name in ("directory-mode", "marker-mode", "marker-symlink", "marker-hardlink", "extra-entry"):
        root = fixture(name)
        command = prepare_backup_command(root, image, engine)
        marker = root / MARKER
        outside = work / (name + "-outside")
        if name == "directory-mode":
            root.chmod(0o777)
        elif name == "marker-mode":
            marker.chmod(0o644)
        elif name == "marker-symlink":
            marker.rename(outside)
            marker.symlink_to(outside)
        elif name == "marker-hardlink":
            os.link(marker, outside)
        else:
            (root / "unexpected").write_text("synthetic")
        before = (root.lstat().st_uid, marker.lstat().st_uid)
        try:
            prepare_backup_command(root, image, engine)
        except RuntimeError:
            pass
        else:
            raise AssertionError("host accepted unsafe fixture: " + name)
        denied = run(command, success=False)
        check("host and container reject unsafe fixture without changing owner: " + name,
              denied.returncode != 0 and before == (root.lstat().st_uid, marker.lstat().st_uid))
        if outside.exists():
            check("outside marker remains owned by caller: " + name, outside.stat().st_uid == os.geteuid())
    return checks


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    require_local_docker(lambda argv: subprocess.check_output(argv, text=True).strip())
    work = Path(tempfile.mkdtemp(prefix="lantai-release-backup-"))
    result = {"schema": "lantai.release-backup-preparation/v1", "status": "running",
              "nas_access": False, "host_uid": os.geteuid(), "checks": []}
    try:
        result["checks"] = run_checks(work, args.image)
        result["status"] = "passed"
    except Exception as error:
        result.update(status="failed", error=str(error))
        raise
    finally:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(json.dumps(result, indent=2) + "\n")


if __name__ == "__main__":
    main()
