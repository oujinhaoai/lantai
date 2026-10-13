#!/usr/bin/env python3
"""Actual exported images/bundles: fresh install and a direct retained-volume upgrade.

Only random disposable Docker projects and synthetic identities are used. Runtime
keys, sessions, enrollment bodies and TLS private keys never enter the evidence.
"""
import argparse
import base64
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import ssl
import struct
import subprocess
import tarfile
import tempfile
import time

from release_bundle import extract_verified_archive, sha256, validate_static, require_local_docker

ROOT = Path(__file__).resolve().parents[1]
ENV = {k: v for k, v in os.environ.items() if not k.startswith(("LANTAI_", "COMPOSE_"))}
RESULT = {"schema": "lantai.release-delivery-smoke/v1", "status": "running",
          "nas_access": False, "production_credentials": False, "checks": []}
PRIVATE_VALUES = []


def run(argv, *, stdin=None, timeout=600):
    result = subprocess.run(argv, input=stdin, text=True, capture_output=True, env=ENV, timeout=timeout)
    if result.returncode:
        RESULT["failed_command"] = {"program": Path(argv[0]).name, "exit_code": result.returncode}
        # No secret-bearing stdout, argv, environment or HTTP body is exported.
        message = result.stderr[:4000]
        for value in PRIVATE_VALUES:
            if value:
                message = message.replace(value, "[REDACTED]")
        RESULT["command_stderr_redacted"] = message
        raise RuntimeError("command failed in " + RESULT.get("phase", "initialization"))
    return result.stdout.strip()


def check(name, condition=True, **details):
    if not condition:
        raise AssertionError(name + (": " + json.dumps(details) if details else ""))
    RESULT["checks"].append({"name": name, "passed": True, **details})
    print("PASS: " + name, flush=True)


def verify_files(directory, manifest):
    seen = set()
    for item in manifest["files"]:
        name = item["name"]
        if Path(name).name != name or name in seen:
            raise RuntimeError("unsafe/duplicate artifact member")
        seen.add(name)
        path = directory / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size != item["size_bytes"] or sha256(path) != item["sha256"]:
            raise RuntimeError("artifact differs from manifest")


def read_member(archive, name):
    with tarfile.open(archive, "r:gz") as tar:
        members = [m for m in tar.getmembers() if m.name == name]
        if len(members) != 1 or not members[0].isfile() or members[0].size > 128 * 1024 * 1024:
            raise RuntimeError("missing or unsafe archive member")
        return tar.extractfile(members[0]).read()


def binary_from(directory, manifest, destination):
    item = next(x for x in manifest["binaries"] if x["platform"] == "linux/amd64")
    body = read_member(directory / item["archive"], "lantai")
    if len(body) != item["size_bytes"] or hashlib.sha256(body).hexdigest() != item["sha256"]:
        raise RuntimeError("CLI differs from manifest")
    destination.write_bytes(body)
    destination.chmod(0o700)


def inspect(cid):
    return json.loads(run(["docker", "inspect", cid]))[0]


def load_image(directory, manifest):
    archive = directory / f"lantai-{manifest['version']}-linux-amd64-image.tar.gz"
    run(["docker", "load", "-i", str(archive)])
    value = json.loads(run(["docker", "image", "inspect", manifest["core_image"]["tag"]]))[0]
    labels = value["Config"]["Labels"]
    check("loaded core archive exact image and source", value["Id"] == manifest["core_image"]["id"]
          and labels["org.opencontainers.image.revision"] == manifest["source_commit"]
          and labels["org.opencontainers.image.version"] == manifest["version"]
          and value["Architecture"] == "amd64" and value["Os"] == "linux")


class Instance:
    def __init__(self, work, config_root, web, caddy, base_compose, initial_caddy, image):
        self.root = work
        self.root.mkdir(mode=0o700)
        self.project = "lantai-delivery-" + secrets.token_hex(6)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        self.origin = "https://localhost:" + str(self.port)
        self.tls = work / "tls"
        self.tls.mkdir(mode=0o700)
        self.setup = work / "setup"
        self.setup.mkdir(mode=0o700)
        run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
             "-nodes", "-days", "1", "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost",
             "-keyout", str(self.tls / "key.pem"), "-out", str(self.tls / "cert.pem")])
        (self.tls / "key.pem").chmod(0o600)
        PRIVATE_VALUES.append((self.tls / "key.pem").read_text())
        self.context = ssl.create_default_context(cafile=str(self.tls / "cert.pem"))
        self.config = work / "config.yaml"
        self.config.write_text((config_root / "config.container.yaml").read_text()
                              .replace("https://library.example", self.origin)
                              .replace("min_free_bytes: 5368709120", "min_free_bytes: 1048576"))
        self.config.chmod(0o644)
        self.config_hash = sha256(self.config)
        self.caddy_file = work / "Caddyfile"
        self.caddy_file.write_bytes(initial_caddy)
        # Test-only loopback publication; ports/configuration stay identical at upgrade.
        base_compose = base_compose.replace('ports: ["${LANTAI_HTTPS_PORT:-443}:443"]',
                                            'ports: ["127.0.0.1:${LANTAI_HTTPS_PORT:-443}:443"]')
        base_compose = base_compose.replace("image: caddy:2.11.4", "image: " + caddy + "\n    pull_policy: never")
        (work / "compose.yaml").write_text(base_compose)
        (work / "compose.web.yaml").write_bytes((config_root / "compose.web.yaml").read_bytes())
        self.env = work / "compose.env"
        self.caddy, self.web = caddy, web
        self.caddy_id = json.loads(run(["docker", "image", "inspect", caddy]))[0]["Id"]
        self.select(image)
        self.compose = ["docker", "compose", "--project-name", self.project, "--env-file",
                        str(self.env), "-f", str(work / "compose.yaml")]
        self.last_step = None
        self.password, self.name = secrets.token_urlsafe(28), "release-reader"
        PRIVATE_VALUES.append(self.password)

    def select(self, image):
        self.env.write_text(f"LANTAI_IMAGE={image}\nLANTAI_CADDY_IMAGE={self.caddy}\n"
                            f"LANTAI_WEB_DIR={self.web}\nLANTAI_SETUP_DIR={self.setup}\n"
                            f"LANTAI_CONFIG_FILE={self.config}\nLANTAI_TLS_DIR={self.tls}\n"
                            f"LANTAI_DOMAIN=localhost\nLANTAI_HTTPS_PORT={self.port}\n")
        self.env.chmod(0o600)

    def change_caddy(self, tag):
        path = self.root / "compose.yaml"
        original = path.read_text()
        old = "image: " + self.caddy
        if original.count(old) != 1:
            raise RuntimeError("expected original gateway image reference")
        path.write_text(original.replace(old, "image: " + tag, 1))
        self.caddy = tag
        self.caddy_id = json.loads(run(["docker", "image", "inspect", tag]))[0]["Id"]

    def request(self, path, payload=None, credential=None, key=None):
        connection = http.client.HTTPSConnection("localhost", self.port, context=self.context, timeout=10)
        headers = {"Origin": self.origin}
        if credential:
            headers["Authorization"] = "Bearer " + credential
        if key:
            headers["Idempotency-Key"] = key
        if payload is not None:
            headers["Content-Type"] = "application/json"
        try:
            connection.request("POST" if payload is not None else "GET", path,
                               json.dumps(payload) if payload is not None else None, headers)
            response = connection.getresponse()
            return response.status, response.read(), dict(response.getheaders())
        finally:
            connection.close()

    def wait(self, path, status):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            try:
                response = self.request(path)
                if response[0] == status:
                    return response[1]
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.3)
        raise AssertionError("expected status never reached: " + path)

    def cid(self):
        return run(self.compose + ["ps", "-a", "-q", "core"])

    def mounts(self):
        return {x["Destination"]: {"type": x["Type"], "source": x.get("Name", x["Source"])}
                for x in inspect(self.cid())["Mounts"] if x["Destination"] in
                {"/var/lib/lantai", "/var/lib/lantai-secrets", "/var/lib/lantai-setup", "/var/lib/lantai/config.yaml"}}

    def totp(self):
        while int(time.time()) // 30 == self.last_step or time.time() % 30 > 20:
            time.sleep(0.2)
        self.last_step = int(time.time()) // 30
        value = hmac.new(self.factor, struct.pack(">Q", self.last_step), hashlib.sha1).digest()
        offset = value[-1] & 15
        return str((struct.unpack(">I", value[offset:offset + 4])[0] & 0x7fffffff) % 1000000).zfill(6)

    def initialize_and_seed(self, cli):
        self.wait("/setup/", 200)
        check("protected setup rejects wrong claim", self.request("/setup/begin", {}, "wrong")[0] == 401)
        claim = self.root / "claim.json"
        run(["docker", "cp", self.cid() + ":/var/lib/lantai-setup/setup-code.json", str(claim)])
        claim.chmod(0o600)
        code = json.loads(claim.read_text())["code"]
        claim.unlink()
        PRIVATE_VALUES.append(code)
        status, body, _ = self.request("/setup/begin", {"name": self.name, "password": self.password}, code)
        check("protected synthetic administrator enrollment", status == 200)
        registration = json.loads(body)
        PRIVATE_VALUES.extend([registration["totp_secret"], registration["enrollment_id"], *registration["recovery_codes"]])
        self.factor = base64.b32decode(registration["totp_secret"])
        status, _, _ = self.request("/setup/confirm", {"enrollment_id": registration["enrollment_id"],
                                  "totp_code": self.totp(), "recovery_codes_saved": True}, code)
        check("original TOTP confirmed without relaxing validation", status == 200)
        self.meta = json.loads(self.wait("/api/v1/meta", 200))
        check("setup closes permanently", self.request("/setup/")[0] == 404)
        status, body, _ = self.request("/api/v1/sessions/login", {
            "name": self.name, "password": self.password, "code": self.totp(), "channel": "cli",
            "scopes": ["admin", "read", "ingest", "organize", "self"], "ttl_seconds": 900})
        check("original synthetic CLI login", status == 201)
        self.token = json.loads(body)["token"]
        PRIVATE_VALUES.append(self.token)
        self.session = self.root / "session.json"
        self.session.write_text(json.dumps({"schema": "lantai.client-session/v1", "origin": self.origin, "token": self.token}))
        self.session.chmod(0o600)
        self.who = self.cli(cli, "whoami")
        project_file = self.root / "project.json"
        project_file.write_text(json.dumps({"key": "release-check", "name": "发行合成资料库", "project_type": "sandbox"}))
        project = self.cli(cli, "project", "create", "--input", str(project_file), "--idempotency-key", "synthetic-release-project")
        status, body, _ = self.request("/api/v1/identity/projects/" + project["project_id"] + "/members", credential=self.token)
        check("normal project membership read", status == 200)
        intent = {"action": "identity.grant_project_role", "command": {
            "project_id": project["project_id"], "expected_revision": json.loads(body)["revision"],
            "principal_id": self.who["principal"]["principal_id"], "role": "owner", "grant": True}}
        status, body, _ = self.request("/api/v1/identity/challenges", intent, self.token)
        check("normal project-owner HumanGrant challenge", status == 201)
        challenge = json.loads(body)
        status, body, _ = self.request("/api/v1/identity/challenges/" + challenge["challenge_id"] + "/verify", {"code": self.totp()}, self.token)
        check("normal project-owner TOTP proof", status == 201)
        status, _, _ = self.request("/api/v1/identity/commands", dict(intent, grant_id=json.loads(body)["grant_id"]), self.token, "synthetic-release-owner")
        check("normal synthetic project role assignment", status == 200)
        inputs = self.root / "input"
        inputs.mkdir()
        self.content = b"# Synthetic release resource\n\nRetained across the direct upgrade.\n"
        (inputs / "README.md").write_bytes(self.content)
        push = self.root / "push.json"
        push.write_text(json.dumps({"project_id": project["project_id"], "slug": "release-sample",
             "content": {"asset_type": "doc", "files": [{"path": "README.md", "role": "primary"}],
             "rights": {"usage": "production", "license": "CC0-1.0", "redistribute_raw": True, "noai": False, "sensitivity": "normal"}}}))
        self.receipt = self.cli(cli, "push", "--input", str(push), "--directory", str(inputs), "--state", str(self.root / "push-state.json"))
        check("baseline actual core accepts synthetic resource", self.receipt["version_number"] == 1)
        return self

    def cli(self, binary, *args):
        return json.loads(run([str(binary), *args, "--server", self.origin, "--ca-file", str(self.tls / "cert.pem"), "--session-file", str(self.session)]))

    def snapshot(self, name):
        snapshot = self.root / name
        data, keys = snapshot / "data", snapshot / "keys"
        data.mkdir(parents=True)
        keys.mkdir()
        run(["docker", "cp", self.cid() + ":/var/lib/lantai/.", str(data)])
        run(["docker", "cp", self.cid() + ":/var/lib/lantai-secrets/.", str(keys)])
        schemas = {}
        for path in data.rglob("*.db"):
            with sqlite3.connect("file:" + str(path) + "?mode=ro", uri=True) as database:
                definition = database.execute("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name").fetchall()
                schemas[str(path.relative_to(data))] = hashlib.sha256(json.dumps(definition).encode()).hexdigest()
        fingerprints = {p.relative_to(keys).as_posix(): sha256(p) for p in keys.rglob("*") if p.is_file()}
        check("synthetic stopped snapshot includes five stores and keys", len(schemas) == 5 and bool(fingerprints))
        return schemas, fingerprints

    def static(self, files):
        for item in files:
            status, body, headers = self.request("/" if item["path"] == "index.html" else "/" + item["path"])
            check("actual gateway bytes match static archive: " + item["path"], status == 200
                  and len(body) == item["size_bytes"] and hashlib.sha256(body).hexdigest() == item["sha256"]
                  and headers.get("Cache-Control") == "no-store"
                  and headers.get("X-Content-Type-Options") == "nosniff"
                  and "frame-ancestors 'none'" in headers.get("Content-Security-Policy", ""))
        check("root and help index resolve", self.request("/")[0] == 200 and self.request("/help/")[0] == 200)
        for path in ("/api/v2/meta", "/api/v1/unknown", "/healthz", "/readyz", "/metrics", "/debug/pprof/", "/unknown"):
            check("API and operations do not fall through: " + path, self.request(path)[0] == 404)
        check("unauthenticated identity denied by actual core", self.request("/api/v1/whoami")[0] == 401)
        gateway = inspect(run(self.compose + ["ps", "-q", "gateway"]))
        check("actual gateway is the exported image", gateway["Image"] == self.caddy_id)
        mounts = [x for x in gateway["Mounts"] if x["Destination"] == "/srv/lantai-web"]
        check("actual gateway static mount is exact and read-only", len(mounts) == 1 and not mounts[0]["RW"] and mounts[0]["Source"] == str(self.web))

    def browser(self, evidence, label):
        public_key = run(["openssl", "x509", "-in", str(self.tls / "cert.pem"), "-pubkey", "-noout"])
        der = subprocess.check_output(["openssl", "pkey", "-pubin", "-outform", "der"], input=public_key.encode(), env=ENV)
        pin = base64.b64encode(hashlib.sha256(der).digest()).decode()
        payload = {"origin": self.origin, "certificate_spki": pin, "name": self.name,
                   "password": self.password, "code": self.totp(), "receipt": self.receipt,
                   "principal_id": self.who["principal"]["principal_id"], "screenshots": str(evidence), "label": label}
        output = run(["node", str(ROOT / "scripts/test-release-browser.cjs")], stdin=json.dumps(payload))
        result = json.loads(output)
        check(label + " actual artifact browser login/home/resource/help/TOTP", result.get("passed") is True
              and result.get("same_principal") is True and result.get("cookie_protected") is True)
        RESULT.setdefault("browser", []).append(result)

    def reduced_session(self):
        status, body, _ = self.request("/api/v1/sessions/login", {
            "name": self.name, "password": self.password, "code": self.totp(),
            "channel": "cli", "scopes": ["self"], "ttl_seconds": 3})
        check("normal reduced-scope synthetic session issued", status == 201)
        token = json.loads(body)["token"]
        PRIVATE_VALUES.append(token)
        status, body, _ = self.request("/api/v1/assets", credential=token)
        check("reduced-scope query exposes no resources", status == 200
              and json.loads(body).get("items") == [], http_status=status)
        status, _, _ = self.request(
            "/api/v1/assets/" + self.receipt["asset_id"] + "/versions/" + self.receipt["version_id"] + "/read-grants",
            {"path": "README.md", "purpose": "archive_review"}, token)
        check("reduced-scope session cannot obtain file authorization", status == 403, http_status=status)
        time.sleep(4)
        check("actual expired session is rejected", self.request(
            "/api/v1/whoami", credential=token)[0] == 401)

    def retained(self, cli, manifest):
        current = json.loads(self.wait("/api/v1/meta", 200))
        check("actual running target core image", inspect(self.cid())["Image"] == manifest["core_image"]["id"])
        check("same instance identity", current["instance_id"] == self.meta["instance_id"])
        check("same administrator and existing CLI session", self.cli(cli, "whoami")["principal"]["principal_id"] == self.who["principal"]["principal_id"])
        check("setup remains closed", self.request("/setup/")[0] == 404)
        check("original config and HTTPS port retained", sha256(self.config) == self.config_hash
              and any(x["HostPort"] == str(self.port) for x in inspect(run(self.compose + ["ps", "-q", "gateway"]))["NetworkSettings"]["Ports"]["443/tcp"]))
        self.cli(cli, "show", "--asset", self.receipt["asset_id"], "--version", self.receipt["version_id"], "--view", "full")
        output = self.root / ("download-" + secrets.token_hex(4))
        output.mkdir()
        self.cli(cli, "pull", "--asset", self.receipt["asset_id"], "--version", self.receipt["version_id"], "--directory", str(output), "--purpose", "archive_review")
        check("original resource version and bytes retained", (output / "README.md").read_bytes() == self.content)

    def ready(self):
        pid = str(inspect(self.cid())["State"]["Pid"])
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            values = {}
            for endpoint in ("healthz", "readyz"):
                result = subprocess.run(["sudo", "nsenter", "--target", pid, "--net", "--", "curl",
                    "--noproxy", "*", "--max-time", "5", "--silent", "--show-error", "--fail",
                    "http://127.0.0.1:9090/" + endpoint], capture_output=True, text=True, env=ENV)
                if result.returncode == 0:
                    values[endpoint] = json.loads(result.stdout)
            if (values.get("healthz", {}).get("live") and values.get("readyz", {}).get("ready")
                    and values["readyz"].get("business_ready") and values["readyz"].get("reason_codes") == []):
                check("actual core loopback health and readiness")
                return
            time.sleep(0.3)
        raise AssertionError("actual core readiness did not converge")

    def tls_negative(self):
        try:
            with socket.create_connection(("127.0.0.1", self.port), timeout=5) as sock:
                self.context.wrap_socket(sock, server_hostname="wrong.invalid")
        except ssl.SSLError:
            # The server may reject unknown SNI before the client sees a leaf;
            # either case must fail after positive CA/hostname checks succeeded.
            check("TLS rejects wrong hostname/SNI")
        else:
            raise AssertionError("TLS hostname validation was bypassed")

    def stop(self):
        run(self.compose + ["stop", "--timeout", "120"])
        core = run(self.compose + ["ps", "-a", "-q", "core"])
        if core:
            check("core exits gracefully", inspect(core)["State"]["ExitCode"] == 0)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--baseline-commit", required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    require_local_docker(run)
    new, old = json.loads(args.manifest.read_text()), json.loads((args.baseline / "manifest.json").read_text())
    RESULT.update(source_commit=new["source_commit"], baseline_commit=old["source_commit"],
                  core_image_id=new["core_image"]["id"], caddy_image_id=new["caddy_image"]["id"],
                  baseline_core_image_id=old["core_image"]["id"], baseline_build_run=old["build_run"])
    RESULT["baseline_caddy_image_id"] = old["caddy_image"]["id"]
    check("baseline exact original release commit", old["source_commit"] == args.baseline_commit)
    verify_files(args.artifacts, new)
    verify_files(args.baseline, old)
    check("actual archives match both manifests")
    load_image(args.artifacts, new)
    load_image(args.baseline, old)
    run(["docker", "load", "-i", str(args.artifacts / "caddy-2.11.4-linux-amd64-image.tar.gz")])
    check("loaded gateway archive exact image", json.loads(run(["docker", "image", "inspect", new["caddy_image"]["tag"]]))[0]["Id"] == new["caddy_image"]["id"])
    run(["docker", "load", "-i", str(args.baseline / "caddy-2.11.4-linux-amd64-image.tar.gz")])
    check("loaded original gateway archive exact image", json.loads(run(["docker", "image", "inspect", old["caddy_image"]["tag"]]))[0]["Id"] == old["caddy_image"]["id"])
    work = Path(tempfile.mkdtemp(prefix="lantai-release-delivery-"))
    work.chmod(0o700)
    instances = []
    evidence = work / "browser-evidence"
    evidence.mkdir()
    try:
        web, deployment = work / "web", work / "deploy"
        for spec, target in ((new["web"], web), (new["deployment"], deployment)):
            extract_verified_archive(args.artifacts / spec["archive"], target, spec["archive_prefix"], spec["files"])
        check("extracted static archive inventory exact", validate_static(web) == new["web"]["files"])
        new_cli, old_cli = work / "new-lantai", work / "old-lantai"
        # Candidate has binaries recorded separately from file archive inventory.
        binary_from(args.artifacts, new, new_cli)
        binary_from(args.baseline, old, old_cli)
        RESULT["phase"] = "fresh install from actual packaged artifacts"
        fresh = Instance(work / "fresh", deployment, web, new["caddy_image"]["tag"],
                         (deployment / "compose.yaml").read_text(), (deployment / "Caddyfile").read_bytes(), new["core_image"]["tag"])
        fresh.compose += ["-f", str(fresh.root / "compose.web.yaml")]
        instances.append(fresh)
        missing = fresh.root / "missing-static"
        fresh.env.write_text(fresh.env.read_text().replace("LANTAI_WEB_DIR=" + str(web), "LANTAI_WEB_DIR=" + str(missing)))
        denied = subprocess.run(fresh.compose + ["up", "-d", "--no-deps", "gateway"], capture_output=True, text=True, env=ENV)
        check("actual Compose rejects missing static directory without creating it", denied.returncode != 0 and not missing.exists())
        fresh.select(new["core_image"]["tag"])
        run(fresh.compose + ["up", "-d"])
        fresh.initialize_and_seed(new_cli)
        fresh.retained(new_cli, new)
        fresh.ready()
        fresh.tls_negative()
        fresh.static(new["web"]["files"])
        fresh.browser(evidence, "fresh")
        fresh.reduced_session()
        run(fresh.compose + ["restart", "core"])
        fresh.retained(new_cli, new)
        RESULT["fresh_install_passed"] = True
        fresh.stop()

        RESULT["phase"] = "original rc.2 actual release baseline"
        old_source = args.baseline / f"lantai-{old['version']}-source.tar.gz"
        original_compose = read_member(old_source, "deploy/compose.yaml").decode()
        original_caddy = read_member(old_source, "deploy/Caddyfile")
        upgrade = Instance(work / "upgrade", deployment, web, old["caddy_image"]["tag"], original_compose, original_caddy, old["core_image"]["tag"])
        instances.append(upgrade)
        run(upgrade.compose + ["up", "-d"])
        upgrade.initialize_and_seed(old_cli)
        upgrade.retained(old_cli, old)
        upgrade.ready()
        check("old release has no static homepage", upgrade.request("/")[0] == 404)
        mounts = upgrade.mounts()
        before_config = json.loads(run(upgrade.compose + ["config", "--format", "json"]))
        upgrade.stop()
        before = upgrade.snapshot("before")

        RESULT["phase"] = "direct rc.2 to target retained-volume upgrade"
        upgrade.select(new["core_image"]["tag"])
        upgrade.change_caddy(new["caddy_image"]["tag"])
        upgrade.select(new["core_image"]["tag"])
        upgrade.caddy_file.write_bytes((deployment / "Caddyfile").read_bytes())
        upgrade.compose += ["-f", str(upgrade.root / "compose.web.yaml")]
        after_config = json.loads(run(upgrade.compose + ["config", "--format", "json"]))
        for service in ("core", "prepare", "gateway"):
            before_config["services"][service]["image"] = after_config["services"][service]["image"]
        added = [v for v in after_config["services"]["gateway"]["volumes"] if v["target"] == "/srv/lantai-web"]
        check("resolved upgrade changes only images and static mount", len(added) == 1)
        before_config["services"]["gateway"]["volumes"].extend(added)
        before_config["services"]["gateway"]["volumes"].sort(key=lambda v: v["target"])
        after_config["services"]["gateway"]["volumes"].sort(key=lambda v: v["target"])
        check("resolved original project/networks/ports/volumes preserved", before_config == after_config)
        run(upgrade.compose + ["up", "-d"])
        run(upgrade.compose + ["up", "-d", "--force-recreate", "gateway"])
        upgrade.retained(new_cli, new)
        upgrade.ready()
        upgrade.tls_negative()
        check("original actual data/key/setup/config mounts preserved", upgrade.mounts() == mounts)
        upgrade.static(new["web"]["files"])
        upgrade.browser(evidence, "direct-upgrade")
        upgrade.reduced_session()
        upgrade.stop()
        after = upgrade.snapshot("after")
        check("database schemas and original key files unchanged", before == after)
        RESULT.update(direct_upgrade_passed=True, original_identity_preserved=True,
                      original_totp_login_passed=True, static_archive_served=True, volumes_preserved=True)

        RESULT["phase"] = "conditional same-pair program/static rollback"
        upgrade.select(old["core_image"]["tag"])
        upgrade.change_caddy(old["caddy_image"]["tag"])
        upgrade.select(old["core_image"]["tag"])
        upgrade.caddy_file.write_bytes(original_caddy)
        upgrade.compose = upgrade.compose[:-2]
        run(upgrade.compose + ["up", "-d"])
        run(upgrade.compose + ["up", "-d", "--force-recreate", "gateway"])
        upgrade.retained(old_cli, old)
        upgrade.ready()
        check("rollback keeps original mounts and removes static exposure", upgrade.mounts() == mounts and upgrade.request("/")[0] == 404)
        RESULT["rollback_passed"] = True
        for instance in instances:
            logs = run(instance.compose + ["logs", "--no-color"])
            check("ordinary container logs exclude all synthetic credentials", not any(x in logs for x in PRIVATE_VALUES if x))
        RESULT["status"] = "passed"
    finally:
        RESULT["completed_phase"] = RESULT.get("phase")
        RESULT["phase"] = "stop disposable containers, retain test volumes"
        for instance in instances:
            try:
                instance.stop()
            except Exception:
                RESULT["cleanup_failed"] = True
                RESULT["status"] = "failed"
        args.out.mkdir(parents=True, exist_ok=True)
        # Screenshots only; never archive the synthetic runtime directories.
        if evidence.exists():
            archive = args.out / "delivery-browser-screenshots.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                for path in sorted(evidence.glob("*.png")):
                    tar.add(path, arcname=path.name)
        (args.out / "delivery-image-smoke.json").write_text(json.dumps(RESULT, ensure_ascii=False, indent=2) + "\n")
    if RESULT["status"] != "passed":
        raise RuntimeError("delivery cleanup or acceptance failed")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        RESULT.update(status="failed", error=type(error).__name__ + ": " + str(error))
        # A failure before the finally block must also remain explicit.
        import sys
        if "--out" in sys.argv:
            out = Path(sys.argv[sys.argv.index("--out") + 1])
            out.mkdir(parents=True, exist_ok=True)
            (out / "delivery-image-smoke.json").write_text(json.dumps(RESULT, ensure_ascii=False, indent=2) + "\n")
        print("FAIL: " + RESULT["error"], flush=True)
        raise SystemExit(1)
