"""Offline release guards and archive integrity; these do not validate Docker."""
import contextlib
import gzip
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("build_release", Path(__file__).parents[1] / "build-release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)
SHA = "a" * 40


class BuildGuards(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "source"
        (self.source / "deploy").mkdir(parents=True)
        (self.source / "deploy/Dockerfile").write_text("FROM golang:1.26.9 AS build\nCOPY api ./api\n")
        (self.source / "deploy/Dockerfile.dockerignore").write_text("**\n!api/**\n")
        (self.source / "go.mod").write_text("module example.test/release\ngo 1.26.0\ntoolchain go1.26.9\n")
        self.out = self.root / "output"
        self.head = SHA
        self.dirty = ""
        self.go = "go version go1.26.9 linux/amd64"
        self.runs = [{"databaseId": 1, "headSha": SHA, "conclusion": "success", "url": "https://example.test/run/1"}]
        self.jobs = [{"name": n, "conclusion": "success"} for n in sorted(release.CI_JOBS)]
        self.calls = []

    def command(self, argv, **kwargs):
        self.calls.append(argv)
        if argv[:3] == ["git", "rev-parse", "HEAD"]:
            return self.head
        if argv[:2] == ["git", "status"]:
            return self.dirty
        if argv[:2] == ["git", "merge-base"]:
            return ""
        if argv[:2] == ["git", "clone"]:
            shutil.copytree(self.source, argv[-1])
            return ""
        if argv[:2] == ["git", "checkout"]:
            return ""
        if argv == ["go", "version"]:
            return self.go
        if argv[:3] == ["gh", "run", "list"]:
            return json.dumps(self.runs)
        if argv[:3] == ["gh", "run", "view"]:
            return json.dumps({"jobs": self.jobs})
        if argv[0] == "docker":
            raise RuntimeError("reached Docker after guards")
        raise AssertionError("unexpected command: " + repr(argv))

    def run_main(self, *extra):
        args = ["build-release.py", "--source", str(self.source), "--commit", SHA,
                "--version", "v0.1.0-rc.1", "--out", str(self.out), "--require-main-ci", *extra]
        with patch.object(sys, "argv", args), patch.dict(os.environ, {"GITHUB_REPOSITORY": "example/repo"}), patch.object(release, "command", self.command), patch.object(release, "gzip_command"):
            release.main()

    def assert_blocked(self, message):
        with self.assertRaisesRegex(RuntimeError, message):
            self.run_main()
        self.assertFalse(self.out.exists())
        self.assertFalse(any(x[0] == "docker" for x in self.calls))

    def test_wrong_or_dirty_source_never_reaches_docker(self):
        self.head = "b" * 40
        self.assert_blocked("commit mismatch")
        self.head = SHA
        self.dirty = " M cmd/lantai/main.go"
        self.assert_blocked("modified source")

    def test_api_input_and_go_versions_are_required(self):
        path = self.source / "deploy/Dockerfile.dockerignore"
        path.write_text("**\n")
        self.assert_blocked("API Docker input repair")
        path.write_text("**\n!api/**\n")
        self.go = "go version go1.25.0 linux/amd64"
        self.assert_blocked("Go 1.26.9")

    def test_ci_must_match_commit_and_every_job(self):
        for runs in [[], [{**self.runs[0], "headSha": "b" * 40}], [{**self.runs[0], "conclusion": "failure"}]]:
            with self.subTest(runs=runs):
                self.runs = runs
                self.assert_blocked("no successful main CI")
        self.runs = [{"databaseId": 1, "headSha": SHA, "conclusion": "success", "url": "https://example.test/run/1"}]
        original = self.jobs
        for jobs in [original[:-1], [{**x, "conclusion": "skipped" if i == 0 else "success"} for i, x in enumerate(original)], [original[0]] * 10]:
            with self.subTest(jobs=jobs):
                self.jobs = jobs
                self.assert_blocked("all ten main CI")

    def test_invalid_inputs_and_existing_output_preserve_files(self):
        for extra in [["--commit", "$(unsafe)"], ["--version", "v0.1.0;unsafe"], ["--out", str(self.source / "artifacts")]]:
            with self.subTest(extra=extra), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                self.run_main(*extra)
        self.out.mkdir()
        marker = self.out / "keep"
        marker.write_text("existing output")
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            self.run_main()
        self.assertEqual(marker.read_text(), "existing output")
        self.assertEqual(self.calls, [])

    def test_valid_gate_reaches_docker_but_failure_has_no_success_manifest(self):
        with self.assertRaisesRegex(RuntimeError, "reached Docker"):
            self.run_main()
        self.assertFalse((self.out / "deliverables/manifest.json").exists())
        failure = json.loads((self.out / "failure.json").read_text())
        self.assertEqual((failure["status"], failure["commit"]), ("failed", SHA))
        ci_call = next(x for x in self.calls if x[:3] == ["gh", "run", "list"])
        for flag, value in [("--commit", SHA), ("--branch", "main"), ("--event", "push")]:
            self.assertEqual(ci_call[ci_call.index(flag) + 1], value)

    def simulate_build_command(self, argv, **kwargs):
        # Test orchestration/provenance only; no real Go or Docker build occurs.
        self.simulated_calls.append((argv, kwargs))
        if argv[0] == "docker":
            if argv[1:3] == ["image", "inspect"]:
                info = {"Os": "linux", "Architecture": "amd64", "Id": "sha256:" + "c" * 64,
                        "RepoDigests": ["example@sha256:" + "d" * 64], "Config": {}}
                if argv[-1].startswith("lantai:"):
                    info["Config"] = {"User": "65532:65532", "Labels": {"org.opencontainers.image.revision": self.image_revision,
                                      "org.opencontainers.image.version": "v0.1.0-rc.1"}}
                return json.dumps([info])
            if argv[1] == "run":
                if argv[-1] == "--json":
                    return json.dumps({"platform": "linux/amd64", "go_version": "go1.26.9"})
                return "v2.11.4 h1:synthetic"
            if argv[1] == "create":
                return "e" * 64
            if argv[1] == "cp":
                Path(argv[-1]).write_bytes(b"synthetic Caddy")
            return "{}"
        if argv[:2] == ["git", "show"]:
            return "12345"
        if argv[:2] == ["go", "build"]:
            destination = Path(argv[argv.index("-o") + 1])
            env = kwargs["env"]
            platform = env["GOOS"], env["GOARCH"]
            self.binary_platforms[str(destination)] = platform
            destination.write_bytes(("synthetic " + "/".join(platform)).encode())
            return ""
        if argv[:3] == ["go", "version", "-m"]:
            goos, goarch = self.binary_platforms[argv[-1]]
            return (argv[-1] + ": go1.26.9\n\tbuild\tvcs.revision=" + SHA +
                    "\n\tbuild\tvcs.modified=false\n\tbuild\tCGO_ENABLED=0\n\tbuild\tGOOS=" + goos + "\n\tbuild\tGOARCH=" + goarch)
        if argv[:2] == ["go", "test"]:
            return "--- PASS: TestCaddySingleHTTPSGateway (0.01s)\nok"
        if argv[0] in self.binary_platforms:
            return json.dumps({"vcs_revision": SHA, "vcs_modified": False,
                               "platform": "linux/amd64", "go_version": "go1.26.9"})
        return self.command(argv, **kwargs)

    def run_simulated_build(self):
        self.binary_platforms = {}
        self.simulated_calls = []
        args = ["build-release.py", "--source", str(self.source), "--commit", SHA,
                "--version", "v0.1.0-rc.1", "--out", str(self.out), "--require-main-ci"]
        (self.source / "LICENSE").write_text("synthetic license")
        with patch.object(sys, "argv", args), patch.dict(os.environ, {"GITHUB_REPOSITORY": "example/repo"}), patch.object(release, "command", self.simulate_build_command), patch.object(release, "gzip_command", side_effect=lambda argv, path, **kw: path.write_bytes(b"synthetic archive")), contextlib.redirect_stdout(io.StringIO()):
            release.main()

    def test_complete_manifest_binds_all_platforms_and_every_deliverable(self):
        self.image_revision = SHA
        self.run_simulated_build()
        deliver = self.out / "deliverables"
        manifest = json.loads((deliver / "manifest.json").read_text())
        self.assertEqual(manifest["source_commit"], SHA)
        self.assertEqual(manifest["main_ci"]["headSha"], SHA)
        self.assertFalse(manifest["nas_deployed"])
        self.assertEqual({x["platform"] for x in manifest["binaries"]}, {"/".join(x) for x in release.PLATFORMS})
        for item in manifest["files"]:
            self.assertEqual(item["size_bytes"], (deliver / item["name"]).stat().st_size)
            self.assertEqual(item["sha256"], release.digest(deliver / item["name"]))
        checksums = {}
        for line in (deliver / "SHA256SUMS").read_text().splitlines():
            digest, name = line.split("  ", 1)
            checksums[name] = digest
            self.assertEqual(digest, release.digest(deliver / name))
        self.assertEqual(set(checksums), {p.name for p in deliver.iterdir()} - {"SHA256SUMS"})
        frozen = self.out.resolve() / "build-work/source"
        docker_build = next(argv for argv, kw in self.simulated_calls if argv[:2] == ["docker", "build"])
        self.assertEqual(docker_build[-1], str(frozen))
        self.assertTrue(all(kw["cwd"] == frozen for argv, kw in self.simulated_calls if argv[:2] == ["go", "build"]))

    def test_wrong_oci_revision_never_exports_a_release_image(self):
        self.image_revision = "b" * 40
        with self.assertRaisesRegex(RuntimeError, "image source labels mismatch"):
            self.run_simulated_build()
        self.assertFalse((self.out / "deliverables/manifest.json").exists())
        self.assertFalse(list((self.out / "deliverables").glob("*image.tar.gz")))


class ArchiveAndImageChecks(unittest.TestCase):
    def test_real_isolated_checkout_omits_ignored_private_files(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            source, dest = root / "source", root / "isolated"
            source.mkdir()
            (source / ".gitignore").write_text("private.txt\n")
            (source / "public.txt").write_text("committed source")
            subprocess.run(["git", "init", "-q", str(source)], check=True)
            subprocess.run(["git", "-C", str(source), "add", "."], check=True)
            subprocess.run(["git", "-C", str(source), "-c", "core.hooksPath=/dev/null", "-c", "user.name=Synthetic test", "-c", "user.email=test@example.invalid", "commit", "--no-gpg-sign", "-qm", "synthetic source"], check=True)
            commit = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
            (source / "private.txt").write_text("fictional ignored secret")
            self.assertEqual(subprocess.check_output(["git", "-C", str(source), "status", "--porcelain"], text=True), "")
            release.clone_committed_source(source, commit, dest)
            self.assertEqual((dest / "public.txt").read_text(), "committed source")
            self.assertFalse((dest / "private.txt").exists())

    def test_buildinfo_requires_clean_commit_toolchain_and_exact_platform(self):
        good = "binary: go1.26.9\n\tbuild\tvcs.revision=" + SHA + "\n\tbuild\tvcs.modified=false\n\tbuild\tCGO_ENABLED=0\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64"
        release.check_buildinfo(good, SHA, "linux", "amd64")
        for old, new in [(SHA, "b" * 40), ("modified=false", "modified=true"), ("go1.26.9", "go1.25.0"), ("CGO_ENABLED=0", "CGO_ENABLED=1"), ("GOARCH=amd64", "GOARCH=arm64")]:
            with self.subTest(old=old), self.assertRaises(RuntimeError):
                release.check_buildinfo(good.replace(old, new), SHA, "linux", "amd64")

    def test_wrong_platform_and_ambiguous_inspections_are_rejected(self):
        for values in [[], [{"Os": "linux", "Architecture": "arm64"}], [{"Os": "windows", "Architecture": "amd64"}], [{"Os": "linux", "Architecture": "amd64"}] * 2]:
            with self.subTest(values=values), patch.object(release, "command", return_value=json.dumps(values)), self.assertRaises(RuntimeError):
                release.image_info("synthetic")

    def test_binary_archive_has_exact_bytes_and_stable_permissions(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            binary, license_file = root / "binary", root / "license"
            binary.write_bytes(b"\x7fELF\x00synthetic binary")
            license_file.write_text("synthetic license\n")
            archives = [root / "a.tar.gz", root / "b.tar.gz"]
            release.write_binary_archive(binary, license_file, archives[0], 12345)
            binary.chmod(0o600)
            license_file.chmod(0o600)
            release.write_binary_archive(binary, license_file, archives[1], 12345)
            self.assertEqual(release.digest(archives[0]), release.digest(archives[1]))
            with tarfile.open(archives[0], "r:gz") as tar:
                self.assertEqual(tar.getnames(), ["lantai", "LICENSE"])
                for name, mode, data in [("lantai", 0o755, binary.read_bytes()), ("LICENSE", 0o644, license_file.read_bytes())]:
                    member = tar.getmember(name)
                    self.assertEqual((member.uid, member.gid, member.mode, member.mtime), (0, 0, mode, 12345))
                    self.assertEqual(tar.extractfile(member).read(), data)

    def test_streamed_archive_preserves_binary_and_reports_command_failure(self):
        with tempfile.TemporaryDirectory() as folder:
            out = Path(folder) / "stream.gz"
            release.gzip_command([sys.executable, "-c", "import sys;sys.stdout.buffer.write(b'\\x00\\xffarchive')"], out)
            self.assertEqual(gzip.decompress(out.read_bytes()), b"\x00\xffarchive")
            with self.assertRaisesRegex(RuntimeError, "archive command failed"):
                release.gzip_command([sys.executable, "-c", "raise SystemExit(3)"], out)


if __name__ == "__main__":
    unittest.main()
