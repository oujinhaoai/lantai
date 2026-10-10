#!/usr/bin/env python3
"""Build packages from an exact clean commit; never publish or deploy them."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import time


def command(argv, *, cwd=None, env=None, capture=True):
    result = subprocess.run(argv, cwd=cwd, env=env, check=True,
                            text=True, capture_output=capture)
    return result.stdout.strip() if capture else ""


def save_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def gzip_command(argv, destination, cwd=None):
    with destination.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as out:
            process = subprocess.Popen(argv, cwd=cwd, stdout=subprocess.PIPE)
            try:
                shutil.copyfileobj(process.stdout, out, 1024 * 1024)
            except BaseException:
                process.terminate()
                process.wait()
                raise
            finally:
                process.stdout.close()
            if process.wait() != 0:
                raise RuntimeError("archive command failed: " + argv[0])


def image_info(reference):
    values = json.loads(command(["docker", "image", "inspect", reference]))
    if len(values) != 1:
        raise RuntimeError("ambiguous image inspection")
    value = values[0]
    if value.get("Os") != "linux" or value.get("Architecture") != "amd64":
        raise RuntimeError("image is not linux/amd64")
    return value


CI_JOBS = {"checks (linux)", "test (ubuntu-latest)", "test (macos-latest)",
           "test (windows-latest)", "build linux/amd64", "build linux/arm64",
           "build darwin/amd64", "build darwin/arm64", "build windows/amd64",
           "build windows/arm64"}
PLATFORMS = [("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"),
             ("darwin", "arm64"), ("windows", "amd64"), ("windows", "arm64")]


def check_ci_jobs(jobs):
    if (len(jobs) != len(CI_JOBS) or {x["name"] for x in jobs} != CI_JOBS
            or any(x["conclusion"] != "success" for x in jobs)):
        raise RuntimeError("expected all ten main CI jobs to succeed")


def write_binary_archive(binary, license_file, destination, stamp, binary_name="lantai"):
    with destination.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as gz, tarfile.open(fileobj=gz, mode="w|") as tar:
        for path, name, mode in [(binary, binary_name, 0o755), (license_file, "LICENSE", 0o644)]:
            info = tar.gettarinfo(str(path), arcname=name)
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mode = mode
            info.mtime = stamp
            with path.open("rb") as stream:
                tar.addfile(info, stream)


def check_buildinfo(buildinfo, commit, goos, goarch):
    settings = {line.strip() for line in buildinfo.splitlines()}
    expected = {"build\tvcs.revision=" + commit, "build\tvcs.modified=false",
                "build\tCGO_ENABLED=0", "build\tGOOS=" + goos,
                "build\tGOARCH=" + goarch}
    if not expected.issubset(settings) or not buildinfo.splitlines()[0].endswith("go1.26.9"):
        raise RuntimeError("binary build info lacks the exact clean commit/toolchain/platform")


def clone_committed_source(source, commit, destination):
    command(["git", "clone", "--no-hardlinks", "--no-checkout", "--", str(source), str(destination)])
    command(["git", "checkout", "--detach", commit], cwd=destination)
    if (command(["git", "rev-parse", "HEAD"], cwd=destination) != commit
            or command(["git", "status", "--porcelain", "--untracked-files=all"], cwd=destination)):
        raise RuntimeError("isolated checkout is not the exact clean commit")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--require-main-ci", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}", args.commit):
        parser.error("commit must be a lowercase 40-character SHA")
    if len(args.version) > 96 or not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?", args.version):
        parser.error("invalid release version")
    source, out = args.source.resolve(), args.out.resolve()
    if source == out or source in out.parents:
        parser.error("output must be outside the source tree")
    if out.exists():
        parser.error("refusing an existing output directory")
    if command(["git", "rev-parse", "HEAD"], cwd=source) != args.commit:
        raise RuntimeError("source commit mismatch")
    if command(["git", "status", "--porcelain", "--untracked-files=all"], cwd=source):
        raise RuntimeError("refusing modified source")
    dockerfile = source / "deploy/Dockerfile"
    allowlist = source / "deploy/Dockerfile.dockerignore"
    original = dockerfile.read_text()
    if "COPY api ./api" not in original.splitlines() or "!api/**" not in allowlist.read_text().splitlines():
        raise RuntimeError("required API Docker input repair is absent")
    source_go = re.search(r"^toolchain go([0-9.]+)$", (source / "go.mod").read_text(), re.M)
    if not source_go or source_go.group(1) != "1.26.9":
        raise RuntimeError("review the changed Go version before building")
    go_version = command(["go", "version"])
    if "go1.26.9 " not in go_version:
        raise RuntimeError("Go 1.26.9 is required")
    ci = None
    if args.require_main_ci:
        repository = os.environ.get("GITHUB_REPOSITORY", "")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise RuntimeError("missing repository identity")
        # workflow checkout fetch-depth=0 supplies origin/main without leaving
        # credentials on disk; fail closed if the ref/object is unavailable.
        command(["git", "merge-base", "--is-ancestor", args.commit, "origin/main"], cwd=source)
        runs = json.loads(command(["gh", "run", "list", "--repo", repository,
                                  "--workflow", "ci.yml", "--commit", args.commit,
                                  "--branch", "main", "--event", "push", "--status", "success",
                                  "--limit", "5", "--json", "databaseId,headSha,conclusion,url"]))
        matches = [x for x in runs if x["headSha"] == args.commit and x["conclusion"] == "success"]
        if not matches:
            raise RuntimeError("no successful main CI for this exact commit")
        ci = matches[0]
        jobs = json.loads(command(["gh", "run", "view", str(ci["databaseId"]),
                                  "--repo", repository, "--json", "jobs"]))["jobs"]
        check_ci_jobs(jobs)
        ci["jobs"] = [{"name": x["name"], "conclusion": x["conclusion"]} for x in jobs]
    out.mkdir(parents=True, mode=0o700)
    deliver = out / "deliverables"
    deliver.mkdir()
    work = out / "build-work"
    work.mkdir()
    try:
        # Git status does not reveal ignored files. Exporting/building the caller
        # checkout could leak an ignored file through COPY or compile extra Go
        # sources. A fresh local clone transfers committed objects only.
        build_source = work / "source"
        clone_committed_source(source, args.commit, build_source)
        original = (build_source / "deploy/Dockerfile").read_text()
        allowlist = build_source / "deploy/Dockerfile.dockerignore"
        source_name = f"lantai-{args.version}-source.tar.gz"
        gzip_command(["git", "archive", "--format=tar", args.commit], deliver / source_name, cwd=build_source)
        docker_version = command(["docker", "version", "--format", "{{json .}}"])
        compose_version = command(["docker", "compose", "version"])
        (deliver / "docker-version.json").write_text(docker_version + "\n")
        (deliver / "compose-version.txt").write_text(compose_version + "\n")
        # Resolve the base, then build from its immutable repository digest.
        command(["docker", "pull", "--platform", "linux/amd64", "golang:1.26.9"], capture=False)
        go_image = image_info("golang:1.26.9")
        repo_digests = go_image.get("RepoDigests") or []
        if not repo_digests:
            raise RuntimeError("Go base has no repository digest")
        base = repo_digests[0]
        old_from = "FROM golang:1.26.9 AS build"
        if original.count(old_from) != 1:
            raise RuntimeError("review the changed Dockerfile before base pinning")
        effective = work / "Dockerfile"
        effective.write_text(original.replace(old_from, f"FROM {base} AS build", 1))
        shutil.copyfile(allowlist, work / "Dockerfile.dockerignore")
        shutil.copyfile(effective, deliver / "Dockerfile.effective.txt")
        shutil.copyfile(allowlist, deliver / "Dockerfile.dockerignore.txt")
        core_tag = f"lantai:{args.version}-{args.commit[:12]}"
        command(["docker", "build", "--platform", "linux/amd64", "--file", str(effective),
                 "--label", "org.opencontainers.image.revision=" + args.commit,
                 "--label", "org.opencontainers.image.version=" + args.version,
                 "--tag", core_tag, str(build_source)], capture=False)
        core = image_info(core_tag)
        labels = core.get("Config", {}).get("Labels") or {}
        if labels.get("org.opencontainers.image.revision") != args.commit or labels.get("org.opencontainers.image.version") != args.version:
            raise RuntimeError("image source labels mismatch")
        if core.get("Config", {}).get("User") != "65532:65532":
            raise RuntimeError("unexpected image user")
        core_version = json.loads(command(["docker", "run", "--rm", "--network", "none", core_tag, "version", "--json"]))
        if core_version.get("platform") != "linux/amd64" or core_version.get("go_version") != "go1.26.9":
            raise RuntimeError("unexpected core image binary platform/toolchain")
        save_json(deliver / "image-version.json", core_version)
        save_json(deliver / "core-image-inspect.json", core)
        save_json(deliver / "go-base-inspect.json", go_image)
        core_archive = f"lantai-{args.version}-linux-amd64-image.tar.gz"
        gzip_command(["docker", "image", "save", core_tag], deliver / core_archive)
        command(["docker", "pull", "--platform", "linux/amd64", "caddy:2.11.4"], capture=False)
        caddy = image_info("caddy:2.11.4")
        if not caddy.get("RepoDigests"):
            raise RuntimeError("Caddy image has no repository digest")
        caddy_tag = "lantai-caddy:2.11.4-" + caddy["Id"].removeprefix("sha256:")[:12]
        command(["docker", "tag", "caddy:2.11.4", caddy_tag])
        caddy_version = command(["docker", "run", "--rm", "--network", "none", "--read-only",
                                 "--cap-drop", "ALL", "--entrypoint", "/bin/sh", caddy_tag,
                                 "-ec", "command -v wget >/dev/null; caddy version"])
        if not caddy_version.startswith("v2.11.4 "):
            raise RuntimeError("unexpected Caddy version")
        (deliver / "caddy-version.txt").write_text(caddy_version + "\n")
        save_json(deliver / "caddy-image-inspect.json", image_info(caddy_tag))
        gzip_command(["docker", "image", "save", caddy_tag], deliver / "caddy-2.11.4-linux-amd64-image.tar.gz")
        # Exercise the repository's real Caddy transport test using the exact
        # binary from the exported image. This remains a synthetic gateway test,
        # not target NAS acceptance or a core container initialization test.
        container = command(["docker", "create", "--entrypoint", "/bin/sh", caddy_tag])
        try:
            caddy_binary = work / "caddy"
            command(["docker", "cp", container + ":/usr/bin/caddy", str(caddy_binary)])
        finally:
            command(["docker", "rm", "--force", container])
        caddy_binary.chmod(0o755)
        gateway_env = os.environ.copy()
        gateway_env.update(LANTAI_TEST_CADDY=str(caddy_binary), GOTOOLCHAIN="local", GOWORK="off", GOFLAGS="")
        gateway_output = command(["go", "test", "-timeout=5m", "-count=1", "-v", "-run",
                                  "^TestCaddySingleHTTPSGateway$", "./deploy"], cwd=build_source, env=gateway_env)
        if "--- PASS: TestCaddySingleHTTPSGateway" not in gateway_output:
            raise RuntimeError("real Caddy gateway test did not pass (skip is not acceptance)")
        (deliver / "gateway-smoke.txt").write_text(gateway_output + "\n")
        stamp = int(command(["git", "show", "-s", "--format=%ct", args.commit], cwd=build_source))
        binaries = []
        for goos, goarch in PLATFORMS:
            binary_name = "lantai.exe" if goos == "windows" else "lantai"
            binary_dir = work / f"{goos}-{goarch}"
            binary_dir.mkdir()
            binary = binary_dir / binary_name
            env = os.environ.copy()
            env.update(CGO_ENABLED="0", GOOS=goos, GOARCH=goarch, GOWORK="off",
                       GOFLAGS="", GOTOOLCHAIN="local")
            env.pop("GOEXPERIMENT", None)
            command(["go", "build", "-trimpath", "-o", str(binary), "./cmd/lantai"], cwd=build_source, env=env, capture=False)
            buildinfo = command(["go", "version", "-m", str(binary)])
            check_buildinfo(buildinfo, args.commit, goos, goarch)
            prefix = f"lantai-{args.version}-{goos}-{goarch}"
            (deliver / f"{prefix}-buildinfo.txt").write_text(buildinfo + "\n")
            binary_archive = deliver / f"{prefix}.tar.gz"
            write_binary_archive(binary, build_source / "LICENSE", binary_archive, stamp, binary_name)
            binaries.append({"platform": f"{goos}/{goarch}", "archive": binary_archive.name,
                             "sha256": digest(binary), "size_bytes": binary.stat().st_size})
            if goos == "linux" and goarch == "amd64":
                version = json.loads(command([str(binary), "version", "--json"]))
                if (version.get("vcs_revision") != args.commit or version.get("vcs_modified") is not False
                        or version.get("platform") != "linux/amd64" or version.get("go_version") != "go1.26.9"):
                    raise RuntimeError("standalone version smoke mismatch")
                save_json(deliver / "binary-version-linux-amd64.json", version)
        if (command(["git", "rev-parse", "HEAD"], cwd=source) != args.commit
                or command(["git", "status", "--porcelain", "--untracked-files=all"], cwd=source)
                or command(["git", "status", "--porcelain", "--untracked-files=all"], cwd=build_source)):
            raise RuntimeError("source changed during build")
        (deliver / "release-images.env").write_text(f"LANTAI_IMAGE={core_tag}\nLANTAI_CADDY_IMAGE={caddy_tag}\n")
        files = [{"name": p.name, "size_bytes": p.stat().st_size, "sha256": digest(p)} for p in sorted(deliver.iterdir())]
        manifest = {"schema": "lantai.release-artifacts/v1", "status": "built-with-cli-smoke",
                    "version": args.version, "source_commit": args.commit, "platform": "linux/amd64",
                    "go_version": go_version, "main_ci": ci,
                    "build_run": {"repository": os.environ.get("GITHUB_REPOSITORY"), "run_id": os.environ.get("GITHUB_RUN_ID"),
                                  "controller_commit": os.environ.get("GITHUB_SHA")},
                    "core_image": {"tag": core_tag, "id": core["Id"], "base_repository_digest": base, "base_image_id": go_image["Id"]},
                    "caddy_image": {"tag": caddy_tag, "id": caddy["Id"], "repository_digests": caddy.get("RepoDigests", [])},
                    "binaries": binaries,
                    "files": files, "nas_deployed": False, "nas_acceptance": "pending",
                    "actual_codex_final_commit_acceptance": "separate evidence required",
                    "gateway_smoke": "real Caddy image binary; synthetic loopback transport test passed",
                    "build_inputs": "fresh local clone of the exact commit; ignored/untracked files are not copied",
                    "note": "Repository digests can name multi-platform indexes; inspected image IDs and platform are retained. Docker image and standalone binary use different VCS contexts."}
        save_json(deliver / "manifest.json", manifest)
        (deliver / "SHA256SUMS").write_text("".join(f"{digest(p)}  {p.name}\n" for p in sorted(deliver.iterdir())))
        print(json.dumps({"status": manifest["status"], "commit": args.commit, "version": args.version,
                          "deliverables": str(deliver), "nas_deployed": False}))
    except Exception as error:
        save_json(out / "failure.json", {"status": "failed", "type": type(error).__name__,
                                         "message": str(error), "commit": args.commit, "at": time.time()})
        raise


if __name__ == "__main__":
    main()
