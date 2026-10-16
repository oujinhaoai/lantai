"""Bounded static/configuration release bundles; no runtime state or secrets."""
import gzip
import hashlib
from html.parser import HTMLParser
import json
import os
from pathlib import Path, PurePosixPath
import re
import tarfile
from urllib.parse import unquote, urlsplit


HELP_PAGES = {"index", "install", "setup", "accounts", "daily", "codex-mcp",
              "upgrade", "backup", "troubleshooting"}


def require_local_docker(run):
    # Inspect context metadata before any pull/load/build/create. Never let a
    # developer's remote Docker context redirect a synthetic test to a NAS.
    endpoint = run(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"])
    effective = os.environ.get("DOCKER_HOST") or endpoint
    if endpoint not in {"unix:///var/run/docker.sock", "unix:///run/docker.sock"} or effective != endpoint:
        raise RuntimeError("release acceptance requires the local Linux Docker socket")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def inventory(root):
    if root.is_symlink() or not root.is_dir():
        raise RuntimeError("bundle root is not a regular directory")
    result = []
    for path in sorted(root.rglob("*")):
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise RuntimeError("bundle contains a link or special file")
        if path.is_file():
            result.append({"path": path.relative_to(root).as_posix(),
                           "size_bytes": path.stat().st_size, "sha256": sha256(path)})
    if not result:
        raise RuntimeError("empty bundle")
    return result


class References(HTMLParser):
    def __init__(self):
        super().__init__()
        self.references = []

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if any(k.lower().startswith("on") or k.lower() == "style" for k in attrs):
            raise RuntimeError("inline executable/style attribute in static HTML")
        if tag == "script" and not attrs.get("src"):
            raise RuntimeError("inline script in static HTML")
        if tag == "style":
            raise RuntimeError("inline style in static HTML")
        for key in ("href", "src"):
            if key in attrs:
                self.references.append((tag, attrs[key]))


def validate_static(root):
    files = inventory(root)
    names = {x["path"] for x in files}
    required = {"index.html", "assets/THIRD_PARTY_NOTICES.txt",
                "help/help.css", "help/help.js", "help/search-data.js", "help/icon.svg"}
    required |= {"help/" + page + ".html" for page in HELP_PAGES}
    if not required.issubset(names):
        raise RuntimeError("static entry, notices or required help pages missing")
    if not any(n.startswith("assets/") and n.endswith(".js") for n in names) or not any(
            n.startswith("assets/") and n.endswith(".css") for n in names):
        raise RuntimeError("production assets missing")
    for item in files:
        name = item["path"]
        parts = PurePosixPath(name).parts
        allowed = name == "index.html" or (
            len(parts) == 2 and parts[0] in {"assets", "help"}
            and Path(name).suffix in {".js", ".css", ".html", ".svg", ".txt"})
        if (not allowed or any(p.startswith(".") for p in parts)
                or item["size_bytes"] > 16 * 1024 * 1024):
            raise RuntimeError("unexpected or oversized static bundle member")
        if name.endswith(".txt") and name != "assets/THIRD_PARTY_NOTICES.txt":
            raise RuntimeError("unexpected static text member")
        if re.search(rb"-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----", (root / name).read_bytes()):
            raise RuntimeError("private-key material in static bundle")
        if name.endswith(".html"):
            parser = References()
            parser.feed((root / name).read_text(encoding="utf-8"))
            for tag, value in parser.references:
                url = urlsplit(value)
                if url.scheme or url.netloc:
                    if tag != "a" or url.scheme != "https":
                        raise RuntimeError("external/unsafe executable static resource")
                    continue
                if not url.path or url.path == "/":
                    continue
                target = ((root / unquote(url.path).lstrip("/")) if url.path.startswith("/")
                          else (root / name).parent / unquote(url.path)).resolve()
                try:
                    target.relative_to(root.resolve())
                except ValueError as error:
                    raise RuntimeError("static reference escapes bundle") from error
                if not target.is_file():
                    raise RuntimeError("static reference missing from bundle")
    return files


def write_tree_archive(root, destination, prefix, stamp, files):
    # Explicit regular-file inventory: no extract-all, links, local caches or
    # ignored files. Gzip and tar metadata do not embed caller paths/timestamps.
    with destination.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as gz, tarfile.open(fileobj=gz, mode="w|") as tar:
        for item in files:
            path = root / item["path"]
            if path.is_symlink() or sha256(path) != item["sha256"]:
                raise RuntimeError("bundle changed during archiving")
            info = tarfile.TarInfo(prefix + "/" + item["path"])
            info.size, info.mode, info.mtime = item["size_bytes"], 0o644, stamp
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            with path.open("rb") as stream:
                tar.addfile(info, stream)


def extract_verified_archive(archive, destination, prefix, files):
    if destination.exists():
        raise RuntimeError("refusing existing bundle extraction directory")
    for item in files:
        path = PurePosixPath(item["path"])
        if path.is_absolute() or not path.parts or ".." in path.parts:
            raise RuntimeError("unsafe manifest path")
    expected = {prefix + "/" + x["path"]: x for x in files}
    if len(expected) != len(files):
        raise RuntimeError("duplicate manifest path")
    destination.mkdir(parents=True)
    with tarfile.open(archive, "r:gz") as tar:
        members = tar.getmembers()
        if len(members) != len(expected) or {m.name for m in members} != set(expected):
            raise RuntimeError("archive member set differs from manifest")
        for member in members:
            if not member.isfile() or member.name.startswith("/") or ".." in PurePosixPath(member.name).parts:
                raise RuntimeError("unsafe archive member")
            item = expected[member.name]
            body = tar.extractfile(member).read()
            if len(body) != item["size_bytes"] or hashlib.sha256(body).hexdigest() != item["sha256"]:
                raise RuntimeError("archive content differs from manifest")
            path = destination / item["path"]
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(body)
            path.chmod(0o644)


def build_web_bundle(source, deliver, version, commit, stamp, run):
    web = source / "web"
    if any(p.name != ".env.example" for p in web.glob(".env*")):
        raise RuntimeError("refusing Vite environment files in release source")
    if (web / "dist").exists():
        raise RuntimeError("refusing an existing dist in isolated source")
    node = run(["node", "--version"])
    if not re.fullmatch(r"v24\.\d+\.\d+", node):
        raise RuntimeError("release static build requires Node 24")
    npm = run(["npm", "--version"])
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("VITE_", "LANTAI_", "COMPOSE_")) and k != "NODE_OPTIONS"}
    steps = [["python3", "docs/user-guide/build.py", "--check"],
             ["python3", "docs/user-guide/check.py"],
             ["npm", "ci", "--prefix", "web"],
             ["npm", "run", "lint", "--prefix", "web"],
             ["npm", "run", "build", "--prefix", "web"]]
    logs = []
    for argv in steps:
        logs.append("$ " + " ".join(argv) + "\n" + run(argv, cwd=source, env=env))
    (deliver / "web-build.txt").write_text("\n\n".join(logs) + "\n")
    files = validate_static(web / "dist")
    archive = deliver / f"lantai-{version}-web.tar.gz"
    write_tree_archive(web / "dist", archive, "web", stamp, files)
    return {"source_commit": commit, "version": version, "node_version": node,
            "npm_version": npm, "lockfile_sha256": sha256(web / "package-lock.json"),
            "archive": archive.name, "archive_sha256": sha256(archive),
            "archive_prefix": "web", "files": files, "build_evidence": "web-build.txt"}


def build_deployment_bundle(source, work, deliver, version, commit, stamp, core_tag, caddy_tag):
    root = work / "deployment"
    root.mkdir()
    for name in ("Caddyfile", "config.container.yaml", "compose.web.yaml", "compose.backup.yaml",
                 "release-install.md", "release-upgrade.md"):
        (root / name).write_bytes((source / "deploy" / name).read_bytes())
    backup_doc = (source / "docs/online-backup.md").read_text()
    backup_doc = backup_doc.replace("(contracts/backup-restore.md)",
        f"(https://github.com/oujinhaoai/lantai/blob/{commit}/docs/contracts/backup-restore.md)")
    backup_doc = backup_doc.replace("(../api/backup-management.md)", "(backup-management-api.md)")
    (root / "online-backup.md").write_text(backup_doc)
    management_doc = (source / "api/backup-management.md").read_text()
    management_doc = management_doc.replace("(openapi.yaml)",
        f"(https://github.com/oujinhaoai/lantai/blob/{commit}/api/openapi.yaml)")
    management_doc = management_doc.replace("(../docs/online-backup.md)", "(online-backup.md)")
    (root / "backup-management-api.md").write_text(management_doc)
    for name in ("release-install.md", "release-upgrade.md"):
        path = root / name
        path.write_text(path.read_text().replace("../docs/online-backup.md", "online-backup.md"))
    compose = (source / "deploy/compose.yaml").read_text()
    if compose.count("image: caddy:2.11.4") != 1:
        raise RuntimeError("review changed Caddy image template")
    compose = compose.replace("image: caddy:2.11.4", "image: ${LANTAI_CADDY_IMAGE:?set the reviewed local Caddy image}\n    pull_policy: never")
    (root / "compose.yaml").write_text(compose)
    (root / "env.example").write_text(
        f"LANTAI_IMAGE={core_tag}\nLANTAI_CADDY_IMAGE={caddy_tag}\n"
        "LANTAI_WEB_DIR=/absolute/path/to/verified/web\n"
        "LANTAI_SETUP_DIR=/absolute/path/to/private/setup\n"
        "LANTAI_CONFIG_FILE=/absolute/path/to/config.container.yaml\n"
        "LANTAI_TLS_DIR=/absolute/path/to/existing/tls\n"
        "LANTAI_DOMAIN=library.example\nLANTAI_HTTPS_PORT=443\n"
        "# Optional: enable compose.backup.yaml once; provision the directory and marker first.\n"
        "# LANTAI_BACKUP_DIR=/absolute/path/to/private/external/backups\n"
        "# LANTAI_BACKUP_ROOT_ID=library-backup-volume-v1\n")
    files = inventory(root)
    archive = deliver / f"lantai-{version}-deployment.tar.gz"
    write_tree_archive(root, archive, "deploy", stamp, files)
    return {"source_commit": commit, "version": version, "archive": archive.name,
            "archive_sha256": sha256(archive), "archive_prefix": "deploy", "files": files,
            "web_mount": {"target": "/srv/lantai-web", "read_only": True,
                          "create_host_path": False, "source_variable": "LANTAI_WEB_DIR"},
            "backup_mount": {"overlay": "compose.backup.yaml", "target": "/var/lib/lantai-backup",
                             "create_host_path": False, "source_variable": "LANTAI_BACKUP_DIR",
                             "identity_variable": "LANTAI_BACKUP_ROOT_ID",
                             "instructions": "online-backup.md"}}
