"""Prepare only the new, private synthetic backup fixture used by release tests."""
import os
from pathlib import Path
import stat


MARKER = ".lantai-backup-root"
PREPARE_SCRIPT = r'''set -eu
test -d /backup && test ! -L /backup
test -f /backup/.lantai-backup-root && test ! -L /backup/.lantai-backup-root
test "$(stat -c '%a:%u:%g' /backup)" = "$1"
test "$(stat -c '%a:%u:%g:%h' /backup/.lantai-backup-root)" = "$2"
test "$(find /backup -mindepth 1 -maxdepth 1 | wc -l)" -eq 1
chown -h 65532:65532 /backup/.lantai-backup-root /backup
test "$(stat -c '%a:%u:%g' /backup)" = '700:65532:65532'
test "$(stat -c '%a:%u:%g:%h' /backup/.lantai-backup-root)" = '600:65532:65532:1'
'''


def prepare_backup_command(root, image, engine=("docker",)):
    root = Path(root)
    if not root.is_absolute() or any(c in str(root) for c in ",\n\r\x00"):
        raise RuntimeError("unsafe synthetic backup fixture path")
    directory = root.lstat()
    if not stat.S_ISDIR(directory.st_mode):
        raise RuntimeError("unsafe synthetic backup fixture directory")
    marker = (root / MARKER).lstat()
    if (not stat.S_ISDIR(directory.st_mode) or stat.S_IMODE(directory.st_mode) != 0o700
            or directory.st_uid != os.geteuid()
            or not stat.S_ISREG(marker.st_mode) or stat.S_IMODE(marker.st_mode) != 0o600
            or marker.st_nlink != 1 or (marker.st_uid, marker.st_gid) != (directory.st_uid, directory.st_gid)
            or {p.name for p in root.iterdir()} != {MARKER}):
        raise RuntimeError("unsafe synthetic backup fixture; refusing ownership change")
    # CHOWN changes owner; DAC_READ_SEARCH permits traversal of the private
    # directory both before and after its owner changes. FOWNER does not permit
    # that traversal. No write-bypass, privileged mode, or source volumes.
    return [*engine, "run", "--rm", "--network", "none", "--read-only", "--user", "0:0",
            "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_READ_SEARCH",
            "--security-opt", "no-new-privileges:true", "--mount",
            f"type=bind,source={root},target=/backup", "--entrypoint", "/bin/sh", image,
            "-ec", PREPARE_SCRIPT, "backup-fixture",
            f"700:{directory.st_uid}:{directory.st_gid}", f"600:{marker.st_uid}:{marker.st_gid}:1"]
