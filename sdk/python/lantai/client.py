"""No retries, redirects, local database access, or alternate permission logic."""
from __future__ import annotations

import hashlib
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import ssl
from urllib.parse import quote, urlencode, urlsplit, unquote
import uuid

from ._contract import API_VERSION, ROUTES

MAX_JSON_BYTES = 8 << 20
_ULID = re.compile(r"[0-7][0-9A-HJKMNP-TV-Z]{25}\Z")


class APIError(Exception):
    """The server's machine-readable error fields, unchanged."""
    def __init__(self, status: int, body: dict):
        self.status = status
        self.body = body
        self.code = body["code"]
        self.retryable = body.get("retryable", False)
        self.recovery_action = body.get("recovery_action")
        self.operation_id = body.get("operation_id")
        super().__init__(f"{self.code}: {body.get('message', '')}")


class ProtocolError(Exception):
    pass


class TransportError(Exception):
    pass


def _id(value: str) -> str:
    if not isinstance(value, str) or not _ULID.fullmatch(value):
        raise ValueError("canonical ULID required")
    return value


def _header(value: str) -> str:
    if not isinstance(value, str) or any(ord(c) < 32 or ord(c) > 126 for c in value):
        raise ValueError("invalid header value")
    return value


def _redact(value, secrets):
    if isinstance(value, str):
        for secret in secrets:
            value = value.replace(secret, "[redacted]")
    elif isinstance(value, dict):
        value = {k: _redact(v, secrets) for k, v in value.items()}
    elif isinstance(value, list):
        value = [_redact(v, secrets) for v in value]
    return value


class Client:
    def __init__(self, origin: str, session_token: str = "", *, allow_http: bool = False,
                 timeout: float = 30):
        u = urlsplit(origin)
        if not u.hostname or u.username is not None or u.password is not None or u.query or u.fragment or u.path not in ("", "/"):
            raise ValueError("server must be an HTTPS origin")
        if u.scheme != "https":
            loopback = u.hostname == "localhost"
            try:
                loopback = loopback or ipaddress.ip_address(u.hostname).is_loopback
            except ValueError:
                pass
            if u.scheme != "http" or not allow_http or not loopback:
                raise ValueError("HTTP is allowed only for explicit loopback development")
        if not math.isfinite(timeout) or timeout <= 0:
            raise ValueError("positive finite timeout required")
        self.origin = f"{u.scheme}://{u.netloc}"
        self._url = u
        self._token = _header(session_token)
        self.timeout = timeout
        self._tls = ssl.create_default_context()

    def _connection(self):
        if self._url.scheme == "https":
            return http.client.HTTPSConnection(self._url.hostname, self._url.port, timeout=self.timeout, context=self._tls)
        return http.client.HTTPConnection(self._url.hostname, self._url.port, timeout=self.timeout)

    def _path(self, value: str, prefix: str) -> str:
        u = urlsplit(value)
        if u.scheme or u.netloc:
            if (u.scheme, u.netloc) != (self._url.scheme, self._url.netloc):
                raise ValueError("cross-origin URL rejected")
        decoded = unquote(u.path)
        if u.fragment or not u.path.startswith(prefix) or any(x in (".", "..") for x in decoded.split("/")) or "\\" in decoded or any(ord(c) < 32 for c in decoded):
            raise ValueError("invalid API or transfer path")
        return u.path + ("?" + u.query if u.query else "")

    def _headers(self):
        return {"Authorization": "Bearer " + self._token} if self._token else {}

    def request(self, method: str, path: str, body=None, *, idempotency_key: str = "", if_match: str = ""):
        method = method.upper()
        path = self._path(path, "/api/v1/")
        clean = urlsplit(path).path
        if not any(method == verb and re.fullmatch(re.sub(r"\\\{[^}]+\\\}", r"[^/]+", re.escape(route)), clean)
                   for verb, route in ROUTES):
            raise ValueError("method or route is absent from the bundled public contract")
        raw = None if body is None else json.dumps(body, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()
        if raw is not None and len(raw) > MAX_JSON_BYTES:
            raise ValueError("JSON request exceeds limit")
        secrets = [self._token] if self._token else []
        if isinstance(body, dict):
            secrets += [body[k] for k in ("token", "password", "code") if isinstance(body.get(k), str) and body[k]]
        headers = self._headers()
        if raw is not None:
            headers["Content-Type"] = "application/json"
        if idempotency_key:
            headers["Idempotency-Key"] = _header(idempotency_key)
        if if_match:
            headers["If-Match"] = _header(if_match)
        conn = self._connection()
        try:
            conn.request(method, path, raw, headers)
            response = conn.getresponse()
            data = response.read(MAX_JSON_BYTES + 1)
            if len(data) > MAX_JSON_BYTES:
                raise ProtocolError("JSON response exceeds limit")
            if 300 <= response.status < 400:
                raise ProtocolError("redirect rejected")
            try:
                value = json.loads(data) if data else {}
            except (ValueError, UnicodeError):
                raise ProtocolError("non-JSON response") from None
            if not 200 <= response.status < 300:
                error = value.get("error") if isinstance(value, dict) else None
                if not isinstance(error, dict) or not isinstance(error.get("code"), str):
                    raise ProtocolError("invalid error envelope")
                # Credentials are never included in exception output even if an
                # intermediary echoes them in an otherwise valid JSON error.
                error = _redact(error, secrets)
                raise APIError(response.status, error)
            return value
        except (OSError, http.client.HTTPException):
            raise TransportError("request failed; reconcile its original idempotency key before retrying") from None
        finally:
            conn.close()

    def meta(self):
        value = self.request("GET", "/api/v1/meta")
        if value.get("api_version") != API_VERSION:
            raise ProtocolError("unsupported API version")
        return value

    def exchange(self, token: str):
        # Session creation must not attach an existing session credential.
        old, self._token = self._token, ""
        try:
            value = self.request("POST", "/api/v1/sessions/exchange", {"token": token, "channel": "cli"})
            self._token = _header(value["token"])
            return value["session"]
        except BaseException:
            self._token = old
            raise

    def whoami(self):
        return self.request("GET", "/api/v1/whoami")

    def search(self, project_id: str, query: str = "", *, cursor: str = "", limit: int = 50):
        self._limit(limit)
        return self.request("GET", "/api/v1/assets?" + urlencode({"project_id": _id(project_id), "q": query, "cursor": cursor, "limit": limit, "view": "brief"}))

    def version(self, asset_id: str, version_id: str, *, full: bool = False):
        return self.request("GET", f"/api/v1/assets/{_id(asset_id)}/versions/{_id(version_id)}?view=" + ("full" if full else "brief"))

    def create_upload(self, project_id: str, files: list[dict], *, key: str):
        return self.request("POST", "/api/v1/uploads", {"project_id": _id(project_id), "files": files}, idempotency_key=key)

    def commit(self, upload_id: str, request: dict, *, key: str):
        return self.request("POST", f"/api/v1/uploads/{_id(upload_id)}/commit", request, idempotency_key=key)

    def tasks(self, project_id: str, *, after: str = "", limit: int = 50):
        self._limit(limit)
        return self.request("GET", "/api/v1/tasks?" + urlencode({"project_id": _id(project_id), "after": _id(after) if after else "", "limit": limit}))

    def task(self, task_id: str):
        return self.request("GET", "/api/v1/tasks/" + _id(task_id))

    def create_task(self, request: dict, *, key: str):
        return self.request("POST", "/api/v1/tasks", request, idempotency_key=key)

    def task_command(self, action: str, request: dict, *, key: str):
        if action not in {"claim", "renew", "release", "submit", "block", "handoff", "assign", "answer", "cancel", "reconcile", "complete", "rework"}:
            raise ValueError("unsupported task action")
        return self.request("POST", "/api/v1/tasks/" + action, request, idempotency_key=key)

    def operation(self, operation_id: str):
        return self.request("GET", "/api/v1/operations/" + _id(operation_id))

    @staticmethod
    def _limit(limit):
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 100:
            raise ValueError("limit must be 1-100")

    def download_file(self, asset_id: str, version_id: str, file_path: str,
                      directory: str | Path, *, purpose: str = "archive_review") -> Path:
        """Stream one exact file, verify its hash, and install without overwriting.

        This minimal SDK does not resume partial files. CLI push/pull owns the
        resumable working-copy workflow.
        """
        parts = PurePosixPath(file_path).parts
        if not parts or file_path.startswith("/") or any(x in ("", ".", "..") or any(c in x for c in '\\<>:"|?*') for x in file_path.split("/")):
            raise ValueError("safe relative file path required")
        version = self.version(asset_id, version_id, full=True)
        files = version["manifest"]["content"]["files"]
        expected = next((f for f in files if f["path"] == file_path), None)
        if expected is None:
            raise ValueError("file is not in the exact manifest")
        grant = self.request("POST", f"/api/v1/assets/{_id(asset_id)}/versions/{_id(version_id)}/read-grants", {"path": file_path, "purpose": purpose})
        if any(grant[k] != expected[k] for k in ("path", "size", "sha256")):
            raise ProtocolError("read grant differs from exact manifest")
        path = self._path(grant["url"], "/xfer/")
        root = Path(directory).resolve(strict=True)
        parent = root
        for part in parts[:-1]:
            parent /= part
            if parent.is_symlink():
                raise ValueError("symlink destination rejected")
            parent.mkdir(exist_ok=True)
        if not parent.resolve().is_relative_to(root):
            raise ValueError("destination escapes directory")
        dest = parent / parts[-1]
        temp = parent / (".lantai-download-" + uuid.uuid4().hex)
        conn = self._connection()
        try:
            conn.request("GET", path, headers=self._headers())
            response = conn.getresponse()
            if response.status != 200:
                raise ProtocolError("download did not return complete bytes")
            count, digest = 0, hashlib.sha256()
            with os.fdopen(os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as out:
                while chunk := response.read(1 << 20):
                    count += len(chunk)
                    if count > expected["size"]:
                        raise ProtocolError("download exceeds declared size")
                    out.write(chunk)
                    digest.update(chunk)
                out.flush()
                os.fsync(out.fileno())
            if count != expected["size"] or digest.hexdigest() != expected["sha256"]:
                raise ProtocolError("download size or hash mismatch")
            os.link(temp, dest)  # fails if a user file or symlink already exists
            return dest
        except (http.client.HTTPException, ConnectionError, TimeoutError):
            raise TransportError("download interrupted; destination was not replaced") from None
        finally:
            conn.close()
            temp.unlink(missing_ok=True)
