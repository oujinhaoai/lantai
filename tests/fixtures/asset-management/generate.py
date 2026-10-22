#!/usr/bin/env python3
"""Create only synthetic asset-management fixtures; never read a source collection."""

from __future__ import annotations

import argparse
import ast
import hashlib
import json
import math
from pathlib import Path, PurePosixPath
import shutil
import struct
import subprocess
import sys
import tempfile
import wave
import zipfile
import zlib

VERSION = "1.0.1"
CONTRACT = "lantai.synthetic-fixtures/v1"
SCRIPT_DIR = Path(__file__).resolve().parent
ROLES = {"primary", "source", "interchange", "texture", "recipe", "record", "preview", "doc"}
KINDS = [
    ("T01", "image", "image"),
    ("T02", "video", "video"),
    ("T03", "audio", "audio"),
    ("T04", "recipe", "config"),
    ("T05", "scene", "scene"),
    ("T06", "model", "model"),
    ("T07", "motion", "motion"),
    ("T08", "restricted-opaque", "model"),
    ("T09", "package", "model"),
]


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def checked_relative(value: str) -> PurePosixPath:
    path = PurePosixPath(value)
    if not value or "\\" in value or path.is_absolute() or any(p in {"", ".", ".."} for p in value.split("/")):
        raise ValueError(f"unsafe relative path: {value!r}")
    return path


def safe_file(root: Path, relative: str) -> Path:
    path = root / checked_relative(relative)
    if not path.resolve().is_relative_to(root.resolve()) or any(p.is_symlink() for p in [path, *path.parents] if p.is_relative_to(root)):
        raise ValueError(f"symlink or escaped path: {relative}")
    return path


def png(path: Path, version: int = 1, width: int = 64, height: int = 64, compression: int = 9) -> None:
    def chunk(kind: bytes, data: bytes) -> bytes:
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    colors = ((220, 65, 50), (45, 100, 225)) if version == 1 else ((40, 185, 95), (230, 190, 25))
    rows = bytearray()
    for y in range(height):
        rows.append(0)
        for x in range(width):
            rows.extend(colors[((x // 8) + (y // 8)) % 2])
    data = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
    data += chunk(b"IDAT", zlib.compress(bytes(rows), compression)) + chunk(b"IEND", b"")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def deterministic_zip(path: Path, entries: dict[str, bytes]) -> None:
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_STORED) as archive:
        for name, content in sorted(entries.items()):
            checked_relative(name)
            info = zipfile.ZipInfo(name, (2000, 1, 1, 0, 0, 0))
            info.external_attr = 0o100644 << 16
            archive.writestr(info, content)


def npy(shape: tuple[int, ...], values: list[float]) -> bytes:
    header = repr({"descr": "<f4", "fortran_order": False, "shape": shape})
    padding = (64 - ((10 + len(header) + 1) % 64)) % 64
    encoded = (header + " " * padding + "\n").encode("ascii")
    return b"\x93NUMPY\x01\x00" + struct.pack("<H", len(encoded)) + encoded + struct.pack("<" + "f" * len(values), *values)


def read_npy(data: bytes) -> tuple[dict, tuple[float, ...]]:
    if data[:8] != b"\x93NUMPY\x01\x00":
        raise ValueError("expected NPY v1.0")
    size = struct.unpack("<H", data[8:10])[0]
    header = ast.literal_eval(data[10:10 + size].decode("ascii").strip())
    if header["descr"] != "<f4" or header["fortran_order"]:
        raise ValueError("unexpected array encoding")
    count = math.prod(header["shape"])
    payload = data[10 + size:]
    if len(payload) != count * 4:
        raise ValueError("array length does not match shape")
    return header, struct.unpack("<" + "f" * count, payload)


def glb(path: Path, version: int) -> None:
    positions = struct.pack("<9f", -1, 0, 0, 1, 0, 0, 0, 0, 1 + version / 10)
    indices = struct.pack("<3H", 0, 1, 2)
    binary = positions + indices
    binary += b"\x00" * ((-len(binary)) % 4)
    doc = {
        "asset": {"version": "2.0", "generator": f"Lantai synthetic fixtures {VERSION}"},
        "scene": 0, "scenes": [{"nodes": [0]}], "nodes": [{"name": "SyntheticTriangle", "mesh": 0}],
        "meshes": [{"primitives": [{"attributes": {"POSITION": 0}, "indices": 1, "material": 0}]}],
        "materials": [{"pbrMetallicRoughness": {"baseColorFactor": [1, 0.2, 0.1, 1] if version == 1 else [0.1, 0.6, 1, 1], "metallicFactor": 0, "roughnessFactor": 0.7}}],
        "buffers": [{"byteLength": len(binary)}],
        "bufferViews": [{"buffer": 0, "byteOffset": 0, "byteLength": 36, "target": 34962}, {"buffer": 0, "byteOffset": 36, "byteLength": 6, "target": 34963}],
        "accessors": [{"bufferView": 0, "componentType": 5126, "count": 3, "type": "VEC3", "min": [-1, 0, 0], "max": [1, 0, 1 + version / 10]}, {"bufferView": 1, "componentType": 5123, "count": 3, "type": "SCALAR"}],
    }
    content = json.dumps(doc, sort_keys=True, separators=(",", ":")).encode()
    content += b" " * ((-len(content)) % 4)
    length = 12 + 8 + len(content) + 8 + len(binary)
    path.write_bytes(struct.pack("<4sII", b"glTF", 2, length) + struct.pack("<I4s", len(content), b"JSON") + content + struct.pack("<I4s", len(binary), b"BIN\x00") + binary)


def run(command: list[str], log: Path, input_bytes: bytes | None = None, timeout: int = 120) -> subprocess.CompletedProcess:
    log.parent.mkdir(parents=True, exist_ok=True)
    try:
        result = subprocess.run(command, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, check=False)
        log.write_bytes(result.stdout)
    except (OSError, subprocess.TimeoutExpired) as error:
        log.write_text(str(error), encoding="utf-8")
        raise RuntimeError(f"tool could not run; see {log.name}") from error
    if result.returncode != 0:
        raise RuntimeError(f"tool exited {result.returncode}; see {log.name}")
    return result


def ffmpeg_run(ffmpeg: str, args: list[str], output: Path, log: Path, input_bytes: bytes | None = None) -> None:
    run([ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", *args, "-map_metadata", "-1", str(output)], log, input_bytes)


def media_frames(version: int) -> bytes:
    digits = ["111101101101111", "010110010010111", "111001111100111", "111001111001111", "101101111001001", "111100111001111", "111100111101111", "111001001001001", "111101111101111", "111101111001111"]
    data = bytearray()
    for frame in range(10):
        for y in range(96):
            for x in range(160):
                color = (int(255 * x / 159), int(255 * y / 95), 50 * version)
                dx, dy = (x - 8) // 4, (y - 8) // 4
                if 0 <= dx < 3 and 0 <= dy < 5 and digits[frame][dy * 3 + dx] == "1":
                    color = (255, 255, 255)
                data.extend(color)
    return bytes(data)


def build_workflow(root: Path, version: int) -> None:
    png(root / "input/synthetic.png", version)
    api = {
        "1": {"class_type": "LoadImage", "inputs": {"image": "synthetic.png"}},
        "2": {"class_type": "ImageScale", "inputs": {"image": ["1", 0], "upscale_method": "nearest-exact", "width": 64 * version, "height": 64, "crop": "disabled"}},
        "3": {"class_type": "SaveImage", "inputs": {"images": ["2", 0], "filename_prefix": f"synthetic-v{version}"}},
    }
    nodes = [
        {"id": 1, "type": "LoadImage", "pos": [0, 0], "size": [300, 300], "flags": {}, "order": 0, "mode": 0, "inputs": [], "outputs": [{"name": "IMAGE", "type": "IMAGE", "links": [1], "slot_index": 0}, {"name": "MASK", "type": "MASK", "links": None, "slot_index": 1}], "properties": {"Node name for S&R": "LoadImage"}, "widgets_values": ["synthetic.png", "image"]},
        {"id": 2, "type": "ImageScale", "pos": [350, 0], "size": [300, 180], "flags": {}, "order": 1, "mode": 0, "inputs": [{"name": "image", "type": "IMAGE", "link": 1}], "outputs": [{"name": "IMAGE", "type": "IMAGE", "links": [2], "slot_index": 0}], "properties": {"Node name for S&R": "ImageScale"}, "widgets_values": ["nearest-exact", 64 * version, 64, "disabled"]},
        {"id": 3, "type": "SaveImage", "pos": [700, 0], "size": [300, 300], "flags": {}, "order": 2, "mode": 0, "inputs": [{"name": "images", "type": "IMAGE", "link": 2}], "outputs": [], "properties": {"Node name for S&R": "SaveImage"}, "widgets_values": [f"synthetic-v{version}"]},
    ]
    write_json(root / "workflow-api.json", api)
    write_json(root / "workflow-ui.json", {"last_node_id": 3, "last_link_id": 2, "nodes": nodes, "links": [[1, 1, 0, 2, 0, "IMAGE"], [2, 2, 0, 3, 0, "IMAGE"]], "groups": [], "config": {}, "extra": {}, "version": 0.4})
    write_json(root / "run.json", {"contract": "synthetic-run/v1", "execution_status": "not_run", "api_workflow": "workflow-api.json", "ui_workflow": "workflow-ui.json", "input_file": "input/synthetic.png", "model_files": [], "required_nodes": ["LoadImage", "ImageScale", "SaveImage"], "comfy_version": "unverified", "note": "Native built-in image nodes, no model or GPU required. Copy input into an isolated ComfyUI input directory before execution."})
    (root / "prompt.txt").write_text(f"Synthetic checker image rescale, version {version}. No model prompt is evaluated.\n", encoding="utf-8")
    (root / "README.md").write_text("# Synthetic recipe\n\nOnly built-in LoadImage → ImageScale → SaveImage nodes. UI/API node IDs and links match. Runtime compatibility must be checked against an isolated installed ComfyUI; JSON checks alone do not prove execution.\n", encoding="utf-8")
    (root / "continue.py").write_text('"""Read-only recipe example; never executed by Lantai."""\nimport json\nfrom pathlib import Path\nprint(json.loads(Path("run.json").read_text())["execution_status"])\n', encoding="utf-8")
    (root / "settings.toml").write_text(f"[synthetic]\nversion = {version}\n", encoding="utf-8")
    (root / "settings.yml").write_text(f"synthetic:\n  version: {version}\n", encoding="utf-8")
    (root / "changes.patch").write_text("--- a/prompt.txt\n+++ b/prompt.txt\n@@ -1 +1 @@\n-Synthetic v1\n+Synthetic v2\n", encoding="utf-8")


def file_role(path: str) -> str:
    suffix = PurePosixPath(path).suffix.lower()
    if "preview" in path:
        return "preview"
    if suffix in {".md", ".txt"}:
        return "doc"
    if "textures/" in path or "input/" in path or suffix == ".hdr":
        return "texture"
    if suffix in {".py", ".toml", ".yml", ".patch"} or path.startswith("workflow-"):
        return "recipe"
    if suffix == ".json":
        return "record"
    if path.startswith("lib/") or suffix in {".uexp", ".strands", ".mtl", ".npz"}:
        return "source"
    if suffix in {".obj", ".fbx"}:
        return "interchange"
    return "primary"


def scan_asset(root: Path) -> list[dict]:
    return [{"path": path.relative_to(root).as_posix(), "role": file_role(path.relative_to(root).as_posix()), "size": path.stat().st_size, "sha256": sha256(path)} for path in sorted(root.rglob("*")) if path.is_file()]


def generate(output: Path, ffmpeg: str, ffprobe: str, blender: str | None, large_image: bool) -> dict:
    if output.exists() and (not output.is_dir() or any(output.iterdir())):
        raise ValueError("output must be a new or empty directory; no existing files are overwritten")
    output.mkdir(parents=True, exist_ok=True)
    tools = {}
    for name, executable in [("ffmpeg", ffmpeg), ("ffprobe", ffprobe)]:
        tools[name] = run([executable, "-version"], output / "logs" / f"{name}-version.txt").stdout.decode(errors="replace").splitlines()[0]
    if blender:
        tools["blender"] = run([blender, "--version"], output / "logs/blender-version.txt").stdout.decode(errors="replace").splitlines()
    tools["python"] = sys.version.split()[0]
    assets = []
    for version in (1, 2):
        for coverage, key, asset_type in KINDS:
            root = output / "assets" / key / f"v{version}"
            root.mkdir(parents=True)
            (root / "LICENSE.txt").write_text("CC0-1.0. Entirely synthetic test fixture; contains no third-party work or real person.\n", encoding="utf-8")
            png(root / "preview.png", version)
            dependencies, entries, limitations = [], [], []
            if key == "image":
                png(root / "design.png", version)
                ffmpeg_run(ffmpeg, ["-i", str(root / "design.png"), "-frames:v", "1", "-threads", "1", "-q:v", "3"], root / "design.jpeg", output / f"logs/image-v{version}.txt")
                (root / "design.svg").write_text(f'<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="{("red" if version == 1 else "blue")}"/></svg>\n', encoding="utf-8")
                entries = ["design.png", "design.jpeg", "design.svg"]
            elif key == "video":
                raw_args = ["-f", "rawvideo", "-pixel_format", "rgb24", "-video_size", "160x96", "-framerate", "10", "-i", "pipe:0", "-f", "lavfi", "-i", f"sine=frequency={440 * version}:sample_rate=8000:duration=1", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-threads", "1", "-c:a", "aac", "-movflags", "+faststart"]
                ffmpeg_run(ffmpeg, raw_args, root / "review.mp4", output / f"logs/video-v{version}.txt", media_frames(version))
                ffmpeg_run(ffmpeg, ["-i", str(root / "review.mp4"), "-c", "copy"], root / "review.mov", output / f"logs/mov-v{version}.txt")
                entries = ["review.mp4", "review.mov"]
            elif key == "audio":
                with wave.open(str(root / "tone.wav"), "wb") as audio:
                    audio.setparams((1, 2, 8000, 8000, "NONE", "not compressed"))
                    audio.writeframes(b"".join(struct.pack("<h", int(10000 * math.sin(2 * math.pi * (440 * version) * i / 8000))) for i in range(8000)))
                ffmpeg_run(ffmpeg, ["-i", str(root / "tone.wav"), "-c:a", "libmp3lame", "-b:a", "32k"], root / "tone.mp3", output / f"logs/audio-v{version}.txt")
                write_json(root / "attribution.json", {"license": "CC0-1.0", "source": "procedural-sine", "attribution_required": False, "voice": False, "frequency_hz": 440 * version})
                entries = ["tone.wav", "tone.mp3"]
            elif key == "recipe":
                build_workflow(root, version)
                entries = ["workflow-ui.json", "workflow-api.json"]
                dependencies = [{"from": "workflow-api.json", "to": "input/synthetic.png", "kind": "runtime-input"}]
                limitations.append("ComfyUI runtime not executed; JSON graph/identity checks only until an isolated runtime is supplied.")
            elif key == "scene":
                png(root / "textures/checker.png", version)
                shutil.copyfile(SCRIPT_DIR / "blender_native.py", root / "continue.py")
                entries = ["scene.blend"]
                dependencies = [{"from": "scene.blend", "to": "textures/checker.png", "kind": "external-texture"}, {"from": "scene.blend", "to": "lib/prop-library.blend", "kind": "linked-library"}]
            elif key == "model":
                glb(root / "model.glb", version)
                png(root / "textures/checker.png", version)
                (root / "model.obj").write_text(f"mtllib model.mtl\no SyntheticTriangle\nv -1 0 0\nv 1 0 0\nv 0 0 {1 + version / 10}\nvt 0 0\nvt 1 0\nvt 0.5 1\nusemtl Checker\nf 1/1 2/2 3/3\n", encoding="utf-8")
                (root / "model.mtl").write_text("newmtl Checker\nKd 1 1 1\nmap_Kd textures/checker.png\n", encoding="utf-8")
                (root / "environment.hdr").write_bytes(b"#?RADIANCE\nFORMAT=32-bit_rle_rgbe\n\n-Y 2 +X 2\n" + bytes([128, 100, 64, 129]) * 4)
                entries = ["model.glb", "model.obj"]
                dependencies = [{"from": "model.obj", "to": "model.mtl", "kind": "material"}, {"from": "model.mtl", "to": "textures/checker.png", "kind": "external-texture"}]
            elif key == "motion":
                deterministic_zip(root / "trajectory.npz", {"positions.npy": npy((3, 3), [0, 0, 0, version, 0, 0, 2 * version, 0, 0]), "frames.npy": npy((3,), [1, 6, 12])})
                write_json(root / "motion.json", {"skeleton": "SyntheticRig", "bone": "Root", "frame_start": 1, "frame_end": 12, "fps": 24, "trajectory": "trajectory.npz", "version": version, "positions_shape": [3, 3], "import_options": {"blender_fbx": {"anim_offset": 0.0}}})
                entries = ["motion.fbx", "trajectory.npz"]
                dependencies = [{"from": "motion.fbx", "to": "motion.json", "kind": "motion-metadata"}, {"from": "motion.json", "to": "trajectory.npz", "kind": "trajectory"}]
            elif key == "restricted-opaque":
                for suffix in ("uasset", "uexp", "uemodel", "ueanim", "strands"):
                    (root / f"synthetic.{suffix}").write_bytes(f"NOT AN ENGINE FILE\nSYNTHETIC OPAQUE {suffix} v{version}\n".encode() + bytes(range(64)))
                write_json(root / "source.json", {"source": "procedural-opaque", "usage": "restricted", "license": "LicenseRef-Synthetic-Reference-Only", "engine_parse_validated": False, "extraction_tool": None})
                entries = ["synthetic.uasset"]
                dependencies = [{"from": "synthetic.uasset", "to": "synthetic.uexp", "kind": "paired-sidecar"}]
                limitations.append("Not native UE files: opaque byte/path/permission tests only; no engine or extraction compatibility claim.")
            elif key == "package":
                deterministic_zip(root / "original-package.zip", {"README.md": f"# Synthetic package v{version}\n".encode(), "textures/checker.png": (output / f"assets/model/v{version}/textures/checker.png").read_bytes(), "model.obj": (output / f"assets/model/v{version}/model.obj").read_bytes(), "model.mtl": (output / f"assets/model/v{version}/model.mtl").read_bytes()})
                entries = ["original-package.zip"]
                write_json(root / "package-index.json", {"archive": "original-package.zip", "members": ["README.md", "model.mtl", "model.obj", "textures/checker.png"], "automatic_extraction": False})
            assets.append({"coverage_id": coverage, "asset_key": key, "asset_type": asset_type, "version_label": f"v{version}", "root": root.relative_to(output).as_posix(), "entrypoints": entries, "dependencies": dependencies, "expected_metadata": {"characters": ["hero-a"], "business_state": "active" if version == 1 else "pending_review", "source": "synthetic", "version_label": f"v{version}", "review_fact": "not_reviewed"}, "rights": {"usage": "restricted" if key == "restricted-opaque" else "production", "license": "LicenseRef-Synthetic-Reference-Only" if key == "restricted-opaque" else "CC0-1.0", "redistribute_raw": key != "restricted-opaque", "noai": key == "restricted-opaque", "sensitivity": "normal"}, "limitations": limitations})
    native_status = {"status": "not_run", "reason": "No Blender executable supplied; native BLEND/FBX creation and native model validation remain unverified."}
    if blender:
        try:
            run([blender, "--background", "--factory-startup", "--python-exit-code", "1", "--python", str(SCRIPT_DIR / "blender_native.py"), "--", "generate", str(output)], output / "logs/blender-generate.txt", timeout=180)
            native_status = {"status": "generated", "reason": "Native BLEND/FBX generated; run verify --native for relocated reopen/continue proof."}
        except RuntimeError as error:
            native_status = {"status": "blocked", "reason": str(error)}
    for asset in assets:
        root = output / asset["root"]
        asset["files"] = scan_asset(root)
        missing = [entry for entry in asset["entrypoints"] if not (root / entry).is_file()]
        asset["status"] = "partial" if missing else "generated"
        asset["missing_entrypoints"] = missing
        if missing:
            asset["limitations"].append("Native entrypoint not generated; support files must not be reported as successful native-format coverage.")
    negative = output / "negatives"
    negative.mkdir()
    (negative / "corrupt.png").write_bytes(b"not a PNG\n")
    (negative / "corrupt.mp4").write_bytes(b"not a movie\n")
    ffmpeg_run(ffmpeg, ["-f", "lavfi", "-i", "testsrc2=size=64x64:rate=2:duration=1", "-c:v", "ffv1", "-threads", "1"], negative / "unsupported-browser-codec.mkv", output / "logs/unsupported-codec.txt")
    write_json(negative / "missing-dependency.json", {"asset_root": "assets/model/v1", "omit": "textures/checker.png", "expected_error": "missing_dependency"})
    write_json(negative / "motion-metadata-mismatch.json", {"fps": 30, "frame_start": 100, "frame_end": 200, "expected_error": "metadata_mismatch"})
    deterministic_zip(negative / "unsafe-path.zip", {"safe.txt": b"not extracted"})
    with zipfile.ZipFile(negative / "unsafe-path.zip", "a") as archive:
        archive.writestr("../outside.txt", b"synthetic unsafe member; never extract")
    if large_image:
        png(negative / "over-preview-limit.png", 1, 2500, 2500, compression=0)
    manifest = {"contract": CONTRACT, "generator_version": VERSION, "generator_sha256": sha256(Path(__file__)), "native_script_sha256": sha256(SCRIPT_DIR / "blender_native.py"), "tools": tools, "native_generation": native_status, "assets": assets, "limitations": ["Generated files are not byte-identical across tool versions, notably BLEND/FBX. Each run records exact file hashes and tool versions.", "No actual Lantai ingest/pull, browser playback, ComfyUI execution, source migration, or backup acceptance is implied by fixture generation."], "negatives": scan_asset(negative)}
    write_json(output / "manifest.json", manifest)
    return manifest


def validate_graph(root: Path) -> None:
    api = json.loads((root / "workflow-api.json").read_text())
    ui = json.loads((root / "workflow-ui.json").read_text())
    nodes = {str(node["id"]): node for node in ui["nodes"]}
    if set(api) != set(nodes) or any(api[key]["class_type"] != nodes[key]["type"] for key in api):
        raise ValueError("ComfyUI UI/API node identity mismatch")
    for link in ui["links"]:
        _, source, output, target, _, _ = link
        if [str(source), output] not in api[str(target)]["inputs"].values():
            raise ValueError("ComfyUI UI/API edge mismatch")
    if not (root / "input" / api["1"]["inputs"]["image"]).is_file():
        raise ValueError("ComfyUI runtime input missing")
    if api["2"]["inputs"]["width"] != nodes["2"]["widgets_values"][1] or api["3"]["inputs"]["filename_prefix"] != nodes["3"]["widgets_values"][0]:
        raise ValueError("ComfyUI UI/API widget mismatch")


def validate_asset(root: Path, asset: dict, allow_partial: bool = False, allow_pull_lockfiles: bool = False) -> list[str]:
    asset_root = safe_file(root, asset["root"])
    declared = set()
    for item in asset["files"]:
        path = safe_file(asset_root, item["path"])
        if item["path"] in declared or item["role"] not in ROLES:
            raise ValueError("duplicate path or invalid role")
        declared.add(item["path"])
        if not path.is_file() or path.stat().st_size != item["size"] or sha256(path) != item["sha256"]:
            raise ValueError(f"file integrity mismatch: {asset['asset_key']}/{item['path']}")
    actual = {path.relative_to(asset_root).as_posix() for path in asset_root.rglob("*") if path.is_file()}
    extras = actual - declared
    allowed_controls = {path + ".lantai-lock" for path in declared} if allow_pull_lockfiles else set()
    if declared - actual or not extras.issubset(allowed_controls):
        raise ValueError("asset contains unlisted or missing files")
    for name in extras:
        control = safe_file(asset_root, name)
        if control.stat().st_size != 0:
            raise ValueError(f"invalid pull lock file: {name}")
    for entry in asset["entrypoints"]:
        if entry not in declared and not (allow_partial and entry in asset["missing_entrypoints"]):
            raise ValueError(f"missing entrypoint: {entry}")
    for dependency in asset["dependencies"]:
        for key in ("from", "to"):
            path = safe_file(asset_root, dependency[key])
            if not path.is_file() and not (allow_partial and dependency[key] in asset["missing_entrypoints"] + ["lib/prop-library.blend"]):
                raise ValueError(f"missing dependency: {dependency[key]}")
    if asset["asset_key"] == "recipe":
        validate_graph(asset_root)
    if asset["asset_key"] == "motion":
        version = int(asset["version_label"][1:])
        with zipfile.ZipFile(asset_root / "trajectory.npz") as archive:
            header, values = read_npy(archive.read("positions.npy"))
            if header["shape"] != (3, 3) or values != (0, 0, 0, version, 0, 0, 2 * version, 0, 0):
                raise ValueError("trajectory differs from known array")
        metadata = json.loads((asset_root / "motion.json").read_text())
        if (metadata["fps"], metadata["frame_start"], metadata["frame_end"]) != (24, 1, 12):
            raise ValueError("motion metadata mismatch")
    if asset["asset_key"] == "package":
        with zipfile.ZipFile(asset_root / "original-package.zip") as archive:
            for name in archive.namelist():
                checked_relative(name)
            if archive.testzip() is not None:
                raise ValueError("archive CRC mismatch")
    return sorted(extras)


def verify(root: Path, ffprobe: str, blender: str | None, native: bool, assets_only: bool = False) -> dict:
    manifest = json.loads((root / "manifest.json").read_text())
    if manifest.get("contract") != CONTRACT:
        raise ValueError("not a synthetic fixture directory")
    records = []
    for asset in manifest["assets"]:
        controls = validate_asset(root, asset, allow_partial=True, allow_pull_lockfiles=assets_only)
        records.append({"coverage_id": asset["coverage_id"], "asset_key": asset["asset_key"], "version_label": asset["version_label"], "integrity": "passed", "native_entrypoints": "blocked" if asset["missing_entrypoints"] else "present", "pull_control_files": controls})
        asset_root = root / asset["root"]
        for entry in asset["entrypoints"]:
            if Path(entry).suffix.lower() in {".png", ".jpeg", ".mp4", ".mov", ".wav", ".mp3"}:
                result = run([ffprobe, "-v", "error", "-show_entries", "stream=codec_name,codec_type,width,height,sample_rate,channels,r_frame_rate:format=duration", "-of", "json", str(asset_root / entry)], root / f"logs/probe-{asset['asset_key']}-{asset['version_label']}-{Path(entry).suffix[1:]}.json")
                probe = json.loads(result.stdout)
                if not probe.get("streams"):
                    raise ValueError("native media probe returned no stream")
                if asset["asset_key"] == "video":
                    video = next(stream for stream in probe["streams"] if stream["codec_type"] == "video")
                    if video["codec_name"] != "h264" or (video["width"], video["height"], video["r_frame_rate"]) != (160, 96, "10/1") or abs(float(probe["format"]["duration"]) - 1) > 0.05:
                        raise ValueError("video known dimensions/codec/timebase mismatch")
    versions = {}
    for asset in manifest["assets"]:
        versions.setdefault(asset["asset_key"], []).append(asset)
    for pair in versions.values():
        if len(pair) != 2 or pair[0]["version_label"] != "v1" or pair[1]["version_label"] != "v2" or pair[0]["files"] == pair[1]["files"]:
            raise ValueError("expected distinct v1/v2 per asset")
    negative_results = []
    if not assets_only:
        for item in manifest["negatives"]:
            path = safe_file(root / "negatives", item["path"])
            if not path.is_file() or path.stat().st_size != item["size"] or sha256(path) != item["sha256"]:
                raise ValueError(f"negative fixture integrity mismatch: {item['path']}")
        for filename in ("corrupt.png", "corrupt.mp4"):
            accepted = False
            try:
                probe = run([ffprobe, "-v", "error", "-show_entries", "stream=codec_type,width,height", "-of", "json", str(root / "negatives" / filename)], root / f"logs/probe-negative-{filename}.txt")
                # FFprobe can return zero for a corrupt .png after creating a
                # zero-dimension stream. Exit code alone is not a decoding proof.
                accepted = any(stream.get("width", 0) > 0 and stream.get("height", 0) > 0 for stream in json.loads(probe.stdout).get("streams", []))
            except (RuntimeError, json.JSONDecodeError):
                pass
            if accepted:
                raise ValueError("corrupt media unexpectedly passed native probe")
            negative_results.append({"case": filename, "result": "native-probe-rejected"})
        unsupported = run([ffprobe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "json", str(root / "negatives/unsupported-browser-codec.mkv")], root / "logs/probe-negative-unsupported.json")
        if json.loads(unsupported.stdout)["streams"][0]["codec_name"] != "ffv1":
            raise ValueError("expected valid unsupported-browser FFV1 fixture")
        negative_results.append({"case": "unsupported-browser-codec.mkv", "result": "valid-ffv1-browser-fallback-not-tested"})
        large = root / "negatives/over-preview-limit.png"
        if large.exists():
            if large.stat().st_size <= 16 * 1024 * 1024:
                raise ValueError("large PNG does not exceed preview size limit")
            probe = run([ffprobe, "-v", "error", "-show_entries", "stream=width,height", "-of", "json", str(large)], root / "logs/probe-negative-over-limit.json")
            if json.loads(probe.stdout)["streams"][0] != {"width": 2500, "height": 2500}:
                raise ValueError("large PNG dimensions differ from expected")
            negative_results.append({"case": "over-preview-limit.png", "result": "valid-large-png-browser-fallback-not-tested"})
        with tempfile.TemporaryDirectory(prefix="lantai-synthetic-negative-") as temp:
            negative_root = Path(temp)
            model = next(asset for asset in manifest["assets"] if asset["asset_key"] == "model" and asset["version_label"] == "v1")
            shutil.copytree(root / model["root"], negative_root / model["root"])
            (negative_root / model["root"] / "textures/checker.png").unlink()
            try:
                validate_asset(negative_root, model)
            except ValueError:
                negative_results.append({"case": "missing-texture", "result": "rejected"})
            else:
                raise ValueError("missing dependency was accepted")
        with zipfile.ZipFile(root / "negatives/unsafe-path.zip") as archive:
            try:
                for name in archive.namelist():
                    checked_relative(name)
            except ValueError:
                negative_results.append({"case": "zip-traversal", "result": "rejected-without-extraction"})
            else:
                raise ValueError("unsafe archive member was accepted")
    else:
        negative_results.append({"case": "fixture-only-negatives", "result": "not-run-not-part-of-service-pull"})
    native_status = {"status": "not_run", "reason": "Native Blender verification not requested."}
    if native:
        if not blender:
            raise ValueError("--native requires --blender")
        if any(asset["missing_entrypoints"] for asset in manifest["assets"]):
            native_status = {"status": "blocked", "reason": "Native generation has missing BLEND/FBX entrypoints; see native_generation and tool log."}
        else:
            # Only generated synthetic files are hidden, not any user data. The old
            # absolute asset path is absent while the relocated subprocess runs.
            with tempfile.TemporaryDirectory(prefix="lantai-synthetic-relocated-", dir=root.parent) as temp:
                relocated = Path(temp)
                for asset in manifest["assets"]:
                    for item in asset["files"]:
                        target = relocated / asset["root"] / item["path"]
                        target.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copyfile(root / asset["root"] / item["path"], target)
                hidden = root / ".source-assets-unavailable"
                if hidden.exists():
                    raise ValueError("unexpected source isolation directory")
                (root / "assets").rename(hidden)
                try:
                    result = run([blender, "--background", "--factory-startup", "--python-exit-code", "1", "--python", str(SCRIPT_DIR / "blender_native.py"), "--", "verify", str(relocated)], root / "logs/blender-relocated-verify.txt", timeout=180)
                    native_status = {"status": "passed", "source_path_unavailable": True, "report": json.loads((relocated / "native-verification.json").read_text()), "tool_exit": result.returncode}
                    # Preserve complete synthetic continuation workspaces, not
                    # only a .blend with missing relative sidecars. They are
                    # evidence/next-version inputs outside the original assets.
                    artifacts = root / "validation-artifacts"
                    artifacts.mkdir(exist_ok=True)
                    destination = Path(tempfile.mkdtemp(prefix="blender-", dir=artifacts))
                    continuations = []
                    for version in (1, 2):
                        workspace = destination / f"assets/scene/v{version}"
                        shutil.copytree(relocated / f"assets/scene/v{version}", workspace)
                        continuations.append({"root": workspace.relative_to(root).as_posix(), "entrypoint": "continued.blend", "files": scan_asset(workspace)})
                    native_status["continued_workspaces"] = continuations
                except RuntimeError as error:
                    native_status = {"status": "blocked", "reason": str(error)}
                finally:
                    hidden.rename(root / "assets")
    result = {"contract": CONTRACT, "generator_version": VERSION, "asset_versions": len(records), "assets": records, "negative_cases": negative_results, "native_verification": native_status, "comfy_execution": "not_run", "opaque_engine_parsing": "not_applicable", "file_count": sum(len(asset["files"]) for asset in manifest["assets"]), "total_bytes": sum(item["size"] for asset in manifest["assets"] for item in asset["files"]), "manifest_sha256": sha256(root / "manifest.json")}
    result["verifier_sha256"] = sha256(Path(__file__))
    write_json(root / "validation.json", result)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("generate")
    create.add_argument("--output", type=Path, required=True)
    create.add_argument("--ffmpeg", default=shutil.which("ffmpeg") or "ffmpeg")
    create.add_argument("--ffprobe", default=shutil.which("ffprobe") or "ffprobe")
    create.add_argument("--blender", help="Installed executable only; omit to explicitly record native BLEND/FBX as blocked")
    create.add_argument("--large-image", action="store_true", help="Also create a valid >16 MiB PNG outside assets for preview-limit tests")
    check = commands.add_parser("verify")
    check.add_argument("--root", type=Path, required=True)
    check.add_argument("--ffprobe", default=shutil.which("ffprobe") or "ffprobe")
    check.add_argument("--blender")
    check.add_argument("--native", action="store_true")
    check.add_argument("--assets-only", action="store_true", help="Verify service-pulled assets without fixture-only negative payloads")
    args = parser.parse_args()
    try:
        if args.command == "generate":
            result = generate(args.output.resolve(), args.ffmpeg, args.ffprobe, args.blender, args.large_image)
            print(json.dumps({"manifest": str(args.output / "manifest.json"), "asset_versions": len(result["assets"]), "native_generation": result["native_generation"]}, ensure_ascii=False))
        else:
            result = verify(args.root.resolve(), args.ffprobe, args.blender, args.native, args.assets_only)
            print(json.dumps({key: value for key, value in result.items() if key != "assets"}, ensure_ascii=False))
            if args.native and result["native_verification"]["status"] != "passed":
                return 1
        return 0
    except (ValueError, RuntimeError, OSError, KeyError, zipfile.BadZipFile) as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
