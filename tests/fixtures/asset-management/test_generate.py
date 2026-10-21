"""Contract and negative tests for generated synthetic fixtures, no user data."""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import zipfile
import zlib

SCRIPT = Path(__file__).with_name("generate.py")
SPEC = importlib.util.spec_from_file_location("asset_fixture_generate", SCRIPT)
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)


class PrimitiveTests(unittest.TestCase):
    def test_png_is_valid_and_deterministic(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture.png(root / "a.png")
            fixture.png(root / "b.png")
            self.assertEqual((root / "a.png").read_bytes(), (root / "b.png").read_bytes())
            data = (root / "a.png").read_bytes()
            self.assertEqual(data[:8], b"\x89PNG\r\n\x1a\n")
            offset, pixels = 8, b""
            while offset < len(data):
                length = struct.unpack(">I", data[offset:offset + 4])[0]
                name = data[offset + 4:offset + 8]
                content = data[offset + 8:offset + 8 + length]
                crc = struct.unpack(">I", data[offset + 8 + length:offset + 12 + length])[0]
                self.assertEqual(zlib.crc32(name + content) & 0xFFFFFFFF, crc)
                if name == b"IDAT":
                    pixels += content
                offset += 12 + length
            self.assertEqual(len(zlib.decompress(pixels)), 64 * (1 + 64 * 3))

    def test_npz_numeric_content_and_archive_reproducibility(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            array = fixture.npy((2, 3), [1, 2, 3, 4, 5, 6])
            for name in ("a.npz", "b.npz"):
                fixture.deterministic_zip(root / name, {"positions.npy": array})
            self.assertEqual(fixture.sha256(root / "a.npz"), fixture.sha256(root / "b.npz"))
            with zipfile.ZipFile(root / "a.npz") as archive:
                header, values = fixture.read_npy(archive.read("positions.npy"))
            self.assertEqual(header["shape"], (2, 3))
            self.assertEqual(values, (1, 2, 3, 4, 5, 6))
            with self.assertRaisesRegex(ValueError, "length"):
                fixture.read_npy(array[:-4])

    def test_glb_buffers_and_accessors_match(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "model.glb"
            fixture.glb(path, 1)
            data = path.read_bytes()
            magic, version, length = struct.unpack("<4sII", data[:12])
            self.assertEqual((magic, version, length), (b"glTF", 2, len(data)))
            json_length, json_type = struct.unpack("<I4s", data[12:20])
            self.assertEqual(json_type, b"JSON")
            doc = json.loads(data[20:20 + json_length])
            binary_offset = 20 + json_length
            binary_length, binary_type = struct.unpack("<I4s", data[binary_offset:binary_offset + 8])
            self.assertEqual(binary_type, b"BIN\x00")
            self.assertEqual(binary_length, doc["buffers"][0]["byteLength"])
            self.assertEqual(struct.unpack("<3H", data[binary_offset + 8 + 36:binary_offset + 8 + 42]), (0, 1, 2))
            self.assertEqual(doc["accessors"][0]["count"], 3)

    def test_paths_reject_traversal_absolute_and_symlinks(self):
        for invalid in ("../a", "/tmp/a", "a/../b", "a//b", "a\\b", "./a", ""):
            with self.subTest(path=invalid), self.assertRaises(ValueError):
                fixture.checked_relative(invalid)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "link").symlink_to(root.parent)
            with self.assertRaisesRegex(ValueError, "symlink|escaped"):
                fixture.safe_file(root, "link/file")

    def test_comfy_workflow_ui_api_edges_and_widgets_match(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture.build_workflow(root, 2)
            fixture.validate_graph(root)
            workflow = json.loads((root / "workflow-ui.json").read_text())
            workflow["nodes"][1]["widgets_values"][1] = 999
            fixture.write_json(root / "workflow-ui.json", workflow)
            with self.assertRaisesRegex(ValueError, "widget mismatch"):
                fixture.validate_graph(root)


class ComfyDriverBoundaryTests(unittest.TestCase):
    def test_evidence_cannot_be_written_inside_installed_source(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "installed-code-placeholder"
            source.mkdir()
            for name in ("nodes.py", "execution.py"):
                (source / name).write_text("# synthetic CLI-boundary placeholder; never imported\n")
            output = source / "do-not-create"
            result = subprocess.run([sys.executable, str(SCRIPT.with_name("comfy_native.py")), "--comfy-source", str(source), "--fixture-root", str(root / "fixtures"), "--output", str(output)], capture_output=True, check=False)
            self.assertEqual(result.returncode, 2)
            self.assertIn(b"separate", result.stderr)
            self.assertFalse(output.exists())

    def test_evidence_cannot_overwrite_existing_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "installed-code-placeholder"
            source.mkdir()
            for name in ("nodes.py", "execution.py"):
                (source / name).write_text("# synthetic CLI-boundary placeholder; never imported\n")
            output = root / "evidence"
            output.mkdir()
            sentinel = output / "keep.txt"
            sentinel.write_bytes(b"preserve this synthetic sentinel")
            result = subprocess.run([sys.executable, str(SCRIPT.with_name("comfy_native.py")), "--comfy-source", str(source), "--fixture-root", str(root / "fixtures"), "--output", str(output)], capture_output=True, check=False)
            self.assertEqual(result.returncode, 2)
            self.assertIn(b"overwritten", result.stderr)
            self.assertEqual(sentinel.read_bytes(), b"preserve this synthetic sentinel")


@unittest.skipUnless(shutil.which("ffmpeg") and shutil.which("ffprobe"), "installed FFmpeg/ffprobe required; no download is attempted")
class GeneratedTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="lantai-synthetic-test-")
        cls.root = Path(cls.temp.name) / "generated"
        cls.manifest = fixture.generate(cls.root, shutil.which("ffmpeg"), shutil.which("ffprobe"), None, False)

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    def test_nine_types_two_versions_roles_and_honest_native_gaps(self):
        self.assertEqual(len(self.manifest["assets"]), 18)
        self.assertEqual({asset["coverage_id"] for asset in self.manifest["assets"]}, {f"T{i:02d}" for i in range(1, 10)})
        self.assertEqual({asset["version_label"] for asset in self.manifest["assets"]}, {"v1", "v2"})
        incomplete = [asset for asset in self.manifest["assets"] if asset["status"] == "partial"]
        self.assertEqual({asset["asset_key"] for asset in incomplete}, {"scene", "motion"})
        for asset in self.manifest["assets"]:
            self.assertTrue({item["role"] for item in asset["files"]}.issubset(fixture.ROLES))
            self.assertEqual(asset["expected_metadata"]["review_fact"], "not_reviewed")
            if asset["asset_key"] == "restricted-opaque":
                self.assertEqual(asset["rights"]["usage"], "restricted")
                self.assertTrue(asset["rights"]["noai"])

    def test_verify_probes_media_integrity_versions_and_negative_cases(self):
        report = fixture.verify(self.root, shutil.which("ffprobe"), None, False)
        self.assertEqual(report["asset_versions"], 18)
        self.assertEqual(report["comfy_execution"], "not_run")
        self.assertEqual(report["opaque_engine_parsing"], "not_applicable")
        self.assertEqual({case["case"] for case in report["negative_cases"]}, {"missing-texture", "zip-traversal", "corrupt.png", "corrupt.mp4", "unsupported-browser-codec.mkv"})

    def test_generate_refuses_to_overwrite(self):
        before = fixture.sha256(self.root / "manifest.json")
        with self.assertRaisesRegex(ValueError, "empty directory"):
            fixture.generate(self.root, "unused", "unused", None, False)
        self.assertEqual(before, fixture.sha256(self.root / "manifest.json"))

    def test_service_pull_verification_does_not_require_uningested_negatives(self):
        with tempfile.TemporaryDirectory(prefix="lantai-synthetic-pull-") as temp:
            root = Path(temp)
            shutil.copytree(self.root / "assets", root / "assets")
            shutil.copyfile(self.root / "manifest.json", root / "manifest.json")
            result = fixture.verify(root, shutil.which("ffprobe"), None, False, assets_only=True)
            self.assertEqual(result["asset_versions"], 18)
            self.assertEqual(result["file_count"], sum(len(asset["files"]) for asset in self.manifest["assets"]))
            self.assertEqual(result["negative_cases"], [{"case": "fixture-only-negatives", "result": "not-run-not-part-of-service-pull"}])
            self.assertFalse((root / "negatives").exists())

    def copy_asset(self, key):
        asset = next(asset for asset in self.manifest["assets"] if asset["asset_key"] == key and asset["version_label"] == "v1")
        temp = tempfile.TemporaryDirectory(prefix="lantai-synthetic-tamper-")
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        shutil.copytree(self.root / asset["root"], root / asset["root"])
        return root, asset

    def test_same_size_tampering_is_rejected(self):
        root, asset = self.copy_asset("image")
        path = root / asset["root"] / "design.png"
        original = path.read_bytes()
        path.write_bytes(original[:-1] + bytes([original[-1] ^ 1]))
        self.assertEqual(path.stat().st_size, len(original))
        with self.assertRaisesRegex(ValueError, "integrity mismatch"):
            fixture.validate_asset(root, asset)

    def test_motion_metadata_mismatch_is_rejected_even_after_rehash(self):
        root, original = self.copy_asset("motion")
        asset = json.loads(json.dumps(original))
        path = root / asset["root"] / "motion.json"
        metadata = json.loads(path.read_text())
        metadata["fps"] = 30
        fixture.write_json(path, metadata)
        asset["files"] = fixture.scan_asset(path.parent)
        with self.assertRaisesRegex(ValueError, "metadata mismatch"):
            fixture.validate_asset(root, asset, allow_partial=True)

    def test_strict_validation_rejects_missing_native_entrypoint(self):
        asset = next(asset for asset in self.manifest["assets"] if asset["asset_key"] == "scene")
        with self.assertRaisesRegex(ValueError, "missing entrypoint"):
            fixture.validate_asset(self.root, asset)

    def test_explicit_native_verification_cli_fails_for_partial_assets(self):
        result = subprocess.run([sys.executable, str(SCRIPT), "verify", "--root", str(self.root), "--native", "--blender", sys.executable], capture_output=True, check=False)
        self.assertEqual(result.returncode, 1, result.stderr.decode())
        self.assertEqual(json.loads(result.stdout)["native_verification"]["status"], "blocked")

    def test_unlisted_file_is_rejected(self):
        root, asset = self.copy_asset("model")
        (root / asset["root"] / "unlisted.txt").write_text("synthetic")
        with self.assertRaisesRegex(ValueError, "unlisted"):
            fixture.validate_asset(root, asset)

    def test_service_pull_allows_only_empty_paired_lockfiles(self):
        root, asset = self.copy_asset("model")
        control = root / asset["root"] / "model.obj.lantai-lock"
        control.write_bytes(b"")
        with self.assertRaisesRegex(ValueError, "unlisted"):
            fixture.validate_asset(root, asset)
        self.assertEqual(fixture.validate_asset(root, asset, allow_pull_lockfiles=True), ["model.obj.lantai-lock"])
        control.write_bytes(b"not a zero-length transfer lock")
        with self.assertRaisesRegex(ValueError, "invalid pull lock"):
            fixture.validate_asset(root, asset, allow_pull_lockfiles=True)

    def test_service_pull_rejects_lockfile_symlink(self):
        root, asset = self.copy_asset("model")
        control = root / asset["root"] / "model.obj.lantai-lock"
        control.symlink_to(root / asset["root"] / "model.obj")
        with self.assertRaisesRegex(ValueError, "symlink"):
            fixture.validate_asset(root, asset, allow_pull_lockfiles=True)

    def test_declared_hashes_match_independent_sha256(self):
        for asset in self.manifest["assets"]:
            for item in asset["files"]:
                content = (self.root / asset["root"] / item["path"]).read_bytes()
                self.assertEqual(item["size"], len(content))
                self.assertEqual(item["sha256"], hashlib.sha256(content).hexdigest())


@unittest.skipUnless(os.environ.get("LANTAI_TEST_BLENDER") and shutil.which("ffmpeg") and shutil.which("ffprobe"), "native Blender generation/reopen requires explicit LANTAI_TEST_BLENDER; no implicit application start")
class NativeWorkflowTests(unittest.TestCase):
    def test_relocated_blender_scene_models_motion_and_continuation(self):
        with tempfile.TemporaryDirectory(prefix="lantai-synthetic-native-test-") as temp:
            root = Path(temp) / "generated"
            blender = os.environ["LANTAI_TEST_BLENDER"]
            manifest = fixture.generate(root, shutil.which("ffmpeg"), shutil.which("ffprobe"), blender, False)
            generation_log = root / "logs/blender-generate.txt"
            self.assertEqual(manifest["native_generation"]["status"], "generated", generation_log.read_text() if generation_log.exists() else manifest["native_generation"])
            self.assertTrue(all(not asset["missing_entrypoints"] for asset in manifest["assets"]))
            before = {item["root"] + "/" + entry["path"]: entry["sha256"] for item in manifest["assets"] for entry in item["files"]}
            result = fixture.verify(root, shutil.which("ffprobe"), blender, True)
            native = result["native_verification"]
            native_log = root / "logs/blender-relocated-verify.txt"
            self.assertEqual(native["status"], "passed", native_log.read_text() if native_log.exists() else native)
            self.assertTrue(native["source_path_unavailable"])
            for version, report in enumerate(native["report"]["versions"], 1):
                self.assertTrue(report["scene"]["rendered"])
                self.assertTrue(report["scene"]["continued_scene_saved"])
                self.assertTrue(report["scene"]["continued_scene_reopened"])
                self.assertTrue(report["model"]["obj_material_loaded"])
                self.assertTrue(report["motion"]["animated"])
                self.assertAlmostEqual(report["motion"]["displacement"], 2 * version, places=2)
            self.assertEqual(len(native["continued_workspaces"]), 2)
            for continued in native["continued_workspaces"]:
                workspace = root / continued["root"]
                self.assertTrue((workspace / continued["entrypoint"]).is_file())
                self.assertTrue((workspace / "lib/prop-library.blend").is_file())
                self.assertTrue((workspace / "textures/checker.png").is_file())
                for item in continued["files"]:
                    self.assertEqual(fixture.sha256(workspace / item["path"]), item["sha256"])
            self.assertTrue((root / "assets").is_dir())
            self.assertFalse((root / ".source-assets-unavailable").exists())
            for relative, expected_hash in before.items():
                self.assertEqual(fixture.sha256(root / relative), expected_hash)
            for asset in manifest["assets"]:
                fixture.validate_asset(root, asset)


if __name__ == "__main__":
    unittest.main()
