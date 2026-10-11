#!/usr/bin/env python3
"""Isolated real-Docker smoke: one up, protected setup, TLS, and restart.

Uses synthetic credentials/certificates in a temporary directory. Never prints
secret-bearing HTTP bodies or container output. No production files are used.
"""
import base64
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import struct
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
# Shell variables override Compose --env-file. Never let a developer's real
# Lantai paths, image or Compose project override this synthetic test.
TEST_ENV = {key: value for key, value in os.environ.items()
            if not key.startswith(("LANTAI_", "COMPOSE_"))}


def command(args):
    result = subprocess.run(args, capture_output=True, text=True, env=TEST_ENV)
    if result.returncode:
        raise RuntimeError("Command failed: " + args[0] + " " + args[1])
    return result.stdout.strip()


def main():
    command(["docker", "compose", "version"])
    command(["docker", "build", "-f", str(ROOT / "deploy/Dockerfile"),
             "-t", "lantai:first-start-smoke", str(ROOT)])
    print("PASS: test image built", flush=True)
    with tempfile.TemporaryDirectory(prefix="lantai-compose-", ignore_cleanup_errors=True) as tmp:
        root = Path(tmp)
        tls = root / "tls"
        tls.mkdir()
        setup = root / "setup"
        setup.mkdir()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        origin = f"https://library.example:{port}"
        command(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt",
                 "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
                 "-subj", "/CN=library.example", "-addext",
                 "subjectAltName=DNS:library.example", "-keyout", str(tls / "key.pem"),
                 "-out", str(tls / "cert.pem")])
        os.chmod(tls / "key.pem", 0o600)
        config = root / "config.yaml"
        config.write_text((ROOT / "deploy/config.container.yaml").read_text()
                          .replace("https://library.example", origin)
                          .replace("min_free_bytes: 5368709120", "min_free_bytes: 1048576"))
        os.chmod(config, 0o644)
        env = root / "compose.env"
        env.write_text(f"LANTAI_IMAGE=lantai:first-start-smoke\nLANTAI_SETUP_DIR={setup}\n"
                       f"LANTAI_CONFIG_FILE={config}\nLANTAI_TLS_DIR={tls}\n"
                       f"LANTAI_DOMAIN=library.example\nLANTAI_HTTPS_PORT={port}\n")
        project = "lantai-smoke-" + secrets.token_hex(6)
        compose = ["docker", "compose", "--project-name", project, "--env-file",
                   str(env), "-f", str(ROOT / "deploy/compose.yaml")]
        context = ssl.create_default_context(cafile=str(tls / "cert.pem"))

        class LocalTLS(http.client.HTTPSConnection):
            def connect(self):
                sock = socket.create_connection(("127.0.0.1", port), timeout=5)
                self.sock = context.wrap_socket(sock, server_hostname="library.example")

        def request(path, payload=None, token=None):
            conn = LocalTLS("library.example", port, context=context, timeout=5)
            headers = {"Origin": origin}
            if token:
                headers["Authorization"] = "Bearer " + token
            if payload is not None:
                headers["Content-Type"] = "application/json"
            try:
                conn.request("POST" if payload is not None else "GET", path,
                             json.dumps(payload) if payload is not None else None, headers)
                response = conn.getresponse()
                return response.status, response.read(), response.getheader("Cache-Control")
            finally:
                conn.close()

        def wait(path, status):
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                try:
                    result = request(path)
                    if result[0] == status:
                        return result
                except (OSError, http.client.HTTPException):
                    pass
                time.sleep(0.25)
            raise AssertionError(f"{path} never returned {status}")

        try:
            # Existing bind path variables must remain effective during upgrades.
            legacy = dict(TEST_ENV, LANTAI_DATA_DIR=str(root / "old-data"),
                          LANTAI_SECRETS_DIR=str(root / "old-secrets"))
            parsed = json.loads(subprocess.check_output(
                compose + ["config", "--format", "json"], env=legacy, text=True))
            for name in ("core", "prepare"):
                mounts = {v["target"]: v for v in parsed["services"][name]["volumes"]}
                assert mounts["/var/lib/lantai"]["source"] == str(root / "old-data")
                assert mounts["/var/lib/lantai-secrets"]["source"] == str(root / "old-secrets")
            startup = subprocess.run(compose + ["up", "-d"], capture_output=True,
                                     text=True, env=TEST_ENV)
            if startup.returncode:
                # Only the first up, before enrollment, may print Docker's
                # startup diagnostics. The prepare helper never reads secrets;
                # do not print core/gateway logs or later command output.
                print(startup.stdout[:8192], flush=True)
                print(startup.stderr[:8192], flush=True)
                diagnostic = subprocess.run(compose + ["logs", "--no-color", "prepare"],
                                            capture_output=True, text=True, env=TEST_ENV)
                print(diagnostic.stdout[:8192], flush=True)
                raise RuntimeError("Initial synthetic Compose startup failed")
            print("PASS: Compose started", flush=True)
            page = wait("/setup/", 200)
            print("PASS: setup reached over verified TLS", flush=True)
            assert page[2] == "no-store"
            assert request("/api/v1/meta")[0] == 404
            assert request("/setup/begin", {}, "wrong")[0] == 401
            cid = command(compose + ["ps", "-q", "core"])
            claim_path = root / "claim.json"
            command(["docker", "cp", cid + ":/var/lib/lantai-setup/setup-code.json", str(claim_path)])
            os.chmod(claim_path, 0o600)
            token = json.loads(claim_path.read_text())["code"]
            claim_path.unlink()
            password = secrets.token_urlsafe(24)
            status, body, _ = request("/setup/begin", {"name": "ada", "password": password}, token)
            assert status == 200
            registration = json.loads(body)
            assert len(registration["recovery_codes"]) == 10
            secret = base64.b32decode(registration["totp_secret"])
            digest = hmac.new(secret, struct.pack(">Q", int(time.time()) // 30), hashlib.sha1).digest()
            offset = digest[-1] & 15
            otp = str((struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7fffffff) % 1000000).zfill(6)
            status, _, _ = request("/setup/confirm", {
                "enrollment_id": registration["enrollment_id"], "totp_code": otp,
                "recovery_codes_saved": True}, token)
            assert status == 200
            wait("/api/v1/meta", 200)
            print("PASS: administrator committed and normal service started", flush=True)
            assert request("/setup/")[0] == 404
            command(compose + ["restart", "core"])
            wait("/api/v1/meta", 200)
            assert request("/setup/")[0] == 404
            command(compose + ["stop"])
            command(compose + ["up", "-d"])
            wait("/api/v1/meta", 200)
            assert request("/setup/")[0] == 404
            logs = command(compose + ["logs", "--no-color"])
            for value in [token, password, registration["totp_secret"], registration["enrollment_id"],
                          *registration["recovery_codes"]]:
                assert value not in logs, "Secret appeared in ordinary container logs"
            print("PASS: real Compose one-up setup, verified TLS, legacy mount preservation, restart idempotence, and log privacy")
        finally:
            # Only this randomly named synthetic test project's volumes.
            command(compose + ["down", "--volumes", "--remove-orphans"])


if __name__ == "__main__":
    main()
