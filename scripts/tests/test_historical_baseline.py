"""Offline guards for the exact historical upgrade-input subset; no Docker or network."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parents[1]))
import historical_baseline as baseline


class HistoricalBaseline(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.directory = self.root / "baseline"
        self.directory.mkdir()
        entries = []
        for name in baseline.INPUTS:
            p = self.directory / name
            p.write_bytes(b"synthetic test archive")
            entries.append(dict(name=name, size_bytes=p.stat().st_size, sha256=baseline.digest(p)))
        self.manifest = dict(version="v0.1.0-rc.5", source_commit=baseline.COMMIT,
                             build_run=dict(repository=baseline.REPOSITORY, run_id=int(baseline.RUN),
                                            controller_commit=baseline.COMMIT), files=entries)
        self.write_manifest()
        (self.directory / "SHA256SUMS").write_text("synthetic checksums\n")
        self.addCleanup(patch.stopall)
        patch.object(baseline, "SUMS_SHA", baseline.digest(self.directory / "SHA256SUMS")).start()
        self.pin_manifest()

    def write_manifest(self):
        (self.directory / "manifest.json").write_text(json.dumps(self.manifest))

    def pin_manifest(self):
        patch.object(baseline, "MANIFEST_SHA", baseline.digest(self.directory / "manifest.json")).start()

    def verify(self):
        return baseline.verify_subset(self.directory, baseline.COMMIT)

    def test_valid_subset_and_wrong_commit(self):
        self.assertEqual(self.verify(), self.manifest)
        with self.assertRaises(RuntimeError):
            baseline.verify_subset(self.directory, "0" * 40)

    def test_missing_extra_and_symlink_rejected(self):
        path = self.directory / baseline.INPUTS[0]
        content = path.read_bytes()
        path.unlink()
        with self.assertRaises(RuntimeError): self.verify()
        path.write_bytes(content)
        extra = self.directory / "extra"
        extra.write_text("extra")
        with self.assertRaises(RuntimeError): self.verify()
        extra.unlink()
        outside = self.root / "outside"
        path.rename(outside)
        path.symlink_to(outside)
        with self.assertRaises(RuntimeError): self.verify()

    def test_same_size_tamper_and_metadata_tamper_rejected(self):
        path = self.directory / baseline.INPUTS[0]
        original = path.read_bytes()
        path.write_bytes(b"x" * len(original))
        with self.assertRaises(RuntimeError): self.verify()
        path.write_bytes(original)
        for name in ("manifest.json", "SHA256SUMS"):
            p = self.directory / name
            original = p.read_bytes()
            p.write_bytes(original + b" ")
            with self.assertRaises(RuntimeError): self.verify()
            p.write_bytes(original)

    def test_manifest_identity_duplicates_and_unsafe_names_rejected(self):
        for change in ("run", "controller", "duplicate", "unsafe"):
            with self.subTest(change=change):
                original = json.loads(json.dumps(self.manifest))
                if change == "run": self.manifest["build_run"]["run_id"] = 1
                if change == "controller": self.manifest["build_run"]["controller_commit"] = "0" * 40
                if change == "duplicate": self.manifest["files"].append(self.manifest["files"][0])
                if change == "unsafe": self.manifest["files"][0]["name"] = "../escape"
                self.write_manifest()
                self.pin_manifest()
                with self.assertRaises(RuntimeError): self.verify()
                self.manifest = original

    def download(self, **overrides):
        args = dict(repository=baseline.REPOSITORY, run=baseline.RUN, artifact=baseline.ARTIFACT,
                    commit=baseline.COMMIT, destination=self.root / "download")
        args.update(overrides)
        return baseline.download(**args)

    def test_custom_tuple_never_uses_subset(self):
        for key in ("repository", "run", "artifact", "commit"):
            with self.subTest(key=key), patch.object(baseline, "gh", return_value="") as gh:
                self.assertEqual(self.download(**{key: "custom"}), "full")
                self.assertEqual(gh.call_count, 1)
                self.assertEqual(gh.call_args.args[:2], ("run", "download"))

    def test_available_original_stays_full_and_download_failure_propagates(self):
        listing = json.dumps([{"artifacts": [{"name": baseline.ARTIFACT, "expired": False}]}])
        with patch.object(baseline, "gh", side_effect=[listing, ""]) as gh:
            self.assertEqual(self.download(), "full")
            self.assertEqual(gh.call_args.args[:2], ("run", "download"))
        with patch.object(baseline, "gh", side_effect=[listing, subprocess.CalledProcessError(1, "gh")]) as gh:
            with self.assertRaises(subprocess.CalledProcessError): self.download()
            self.assertEqual(gh.call_count, 2)

    def test_api_auth_network_or_malformed_response_no_fallback(self):
        for failure in (subprocess.CalledProcessError(1, "gh"), "not-json"):
            with patch.object(baseline, "gh") as gh:
                if isinstance(failure, Exception): gh.side_effect = failure
                else: gh.return_value = failure
                with self.assertRaises((subprocess.CalledProcessError, ValueError)): self.download()
                self.assertEqual(gh.call_count, 1)

    def test_missing_or_expired_downloads_only_exact_assets_and_verifies(self):
        for artifacts in ([], [{"name": baseline.ARTIFACT, "expired": True}]):
            with tempfile.TemporaryDirectory() as tmp:
                destination = Path(tmp) / "download"
                with patch.object(baseline, "gh", side_effect=[json.dumps([{"artifacts": artifacts}]), ""]) as gh, patch.object(baseline, "verify_subset") as verify:
                    self.assertEqual(self.download(destination=destination), baseline.SCOPE)
                    call = gh.call_args.args
                    self.assertEqual(call[:3], ("release", "download", baseline.TAG))
                    self.assertEqual(call.count("--pattern"), 7)
                    self.assertEqual(set(call[i+1] for i, x in enumerate(call) if x == "--pattern"), set(baseline.ASSETS))
                    verify.assert_called_once_with(destination, baseline.COMMIT)

    def test_incomplete_listing_never_allows_fallback(self):
        for pages in ([], {}, [{"artifacts": None}], [{"artifacts": [{"name": baseline.ARTIFACT, "expired": "false"}]}]):
            with self.subTest(pages=pages), patch.object(baseline, "gh", return_value=json.dumps(pages)) as gh:
                with self.assertRaises(RuntimeError): self.download()
                self.assertEqual(gh.call_count, 1)

    def test_existing_destination_and_ambiguous_listing_rejected(self):
        with self.assertRaises(RuntimeError): self.download(destination=self.directory)
        a = {"name": baseline.ARTIFACT, "expired": False}
        with patch.object(baseline, "gh", return_value=json.dumps([{"artifacts": [a, a]}])) as gh:
            with self.assertRaises(RuntimeError): self.download()
            self.assertEqual(gh.call_count, 1)


if __name__ == "__main__":
    unittest.main()
