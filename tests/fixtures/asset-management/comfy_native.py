#!/usr/bin/env python3
"""Run the synthetic no-model recipe in an explicitly supplied installed ComfyUI.

Uses ComfyUI's real validator, executor and nodes with an in-memory event sink.
Does not start a server, load custom nodes, invoke a package manager or open a
desktop session. Python audit guards are additional checks, not a sandbox.
"""

import argparse
import asyncio
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile

sys.dont_write_bytecode = True


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class EventSink:
    """ComfyUI ExecutionServer protocol without any socket or HTTP listener."""

    client_id = None
    last_node_id = None
    sockets_metadata = {}

    def __init__(self):
        self.events = []

    def send_sync(self, event, data, sid=None):
        self.events.append({"event": event, "data": data, "sid": sid})

    def queue_updated(self):
        pass


def audit_guard(source, fixtures, output):
    allowed_reads = [source, fixtures, output, Path(__file__).resolve().parent, Path(sys.prefix).resolve(), Path(sys.base_prefix).resolve(), Path("/System"), Path("/usr"), Path("/etc"), Path("/private/etc"), Path("/dev"), Path("/opt/homebrew")]
    forbidden_models = {".safetensors", ".ckpt", ".pth", ".pt", ".gguf", ".onnx"}
    records = {"blocked": [], "file_open_events": 0, "directory_events": 0, "socket_listen_or_connect_events": 0}

    def guard(event, args):
        if event in {"socket.connect", "socket.bind", "socket.getaddrinfo", "subprocess.Popen", "os.system"}:
            if event.startswith("socket."):
                records["socket_listen_or_connect_events"] += 1
            records["blocked"].append({"event": event})
            raise PermissionError(f"synthetic ComfyUI run forbids {event}")
        if event in {"os.mkdir", "os.remove", "os.rmdir", "os.rename", "os.symlink", "os.link"}:
            paths = args[:2] if event in {"os.rename", "os.symlink", "os.link"} else args[:1]
            if any(not Path(os.fsdecode(path)).resolve().is_relative_to(output) for path in paths):
                records["blocked"].append({"event": event, "reason": "mutation-outside-output", "paths": [os.fsdecode(path) for path in paths]})
                raise PermissionError("synthetic ComfyUI filesystem mutations must stay inside evidence")
        if event not in {"open", "os.listdir", "os.scandir", "sqlite3.connect"} or not args or isinstance(args[0], int):
            return
        path = Path(os.fsdecode(args[0])).resolve()
        if event == "open":
            records["file_open_events"] += 1
            mode = args[1] if len(args) > 1 else None
            flags = args[2] if len(args) > 2 else 0
            writing = bool(mode and any(char in mode for char in "wax+")) or bool(isinstance(flags, int) and flags & (os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_TRUNC | os.O_APPEND))
            if writing and not path.is_relative_to(output):
                records["blocked"].append({"event": event, "reason": "write-outside-output", "path": str(path)})
                raise PermissionError("synthetic ComfyUI writes must stay inside its new evidence directory")
        else:
            records["directory_events"] += 1
        if path.suffix.lower() in forbidden_models or not any(path.is_relative_to(root) for root in allowed_reads):
            records["blocked"].append({"event": event, "reason": "outside-code-runtime-or-synthetic-roots", "path": str(path)})
            raise PermissionError("synthetic ComfyUI run forbids model or unrelated source reads")

    sys.addaudithook(guard)
    return records


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--comfy-source", type=Path, required=True, help="Installed source root containing nodes.py and execution.py")
    parser.add_argument("--fixture-root", type=Path, required=True, help="Generated or actual Lantai-pulled synthetic corpus")
    parser.add_argument("--output", type=Path, required=True, help="New or empty evidence directory only")
    args = parser.parse_args()
    source, fixtures, output = args.comfy_source.resolve(), args.fixture_root.resolve(), args.output.resolve()
    if not (source / "nodes.py").is_file() or not (source / "execution.py").is_file():
        parser.error("installed ComfyUI nodes.py and execution.py are required")
    if output.is_relative_to(source) or output.is_relative_to(fixtures):
        parser.error("evidence output must be separate from installed code and source fixtures")
    if output.exists() and (not output.is_dir() or any(output.iterdir())):
        parser.error("output must be new or empty; no existing files are overwritten")
    output.mkdir(parents=True, exist_ok=True)
    manifest = json.loads((fixtures / "manifest.json").read_text())
    if manifest.get("contract") != "lantai.synthetic-fixtures/v1":
        parser.error("only the synthetic fixture corpus is accepted")
    recipes = [asset for asset in manifest["assets"] if asset["asset_key"] == "recipe"]
    if {asset["version_label"] for asset in recipes} != {"v1", "v2"} or len(recipes) != 2:
        parser.error("exactly two synthetic recipe versions are required")
    for asset in recipes:
        asset_root = fixtures / asset["root"]
        if not asset_root.resolve().is_relative_to(fixtures):
            parser.error("synthetic root escaped fixture corpus")
        for item in asset["files"]:
            path = (asset_root / item["path"]).resolve()
            if not path.is_relative_to(asset_root.resolve()) or path.stat().st_size != item["size"] or digest(path) != item["sha256"]:
                parser.error("synthetic recipe integrity mismatch")
        for name in ("input", "output", "user", "temp", "models"):
            (output / asset["version_label"] / name).mkdir(parents=True)
        shutil.copyfile(asset_root / "input/synthetic.png", output / asset["version_label"] / "input/synthetic.png")
    runtime = output / "runtime"
    for name in ("input", "output", "user", "temp", "models", "cache"):
        (runtime / name).mkdir(parents=True)
    for name in ("TORCH_HOME", "HF_HOME", "XDG_CACHE_HOME", "MPLCONFIGDIR", "TRITON_CACHE_DIR", "TORCHINDUCTOR_CACHE_DIR", "NUMBA_CACHE_DIR"):
        os.environ[name] = str(runtime / "cache" / name.lower())
    tempfile.tempdir = str(runtime / "temp")
    os.environ.update({"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "HF_HUB_DISABLE_TELEMETRY": "1", "DO_NOT_TRACK": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    sys.path.insert(0, str(source))
    sys.argv = ["synthetic-comfy-run", "--base-directory", str(runtime), "--input-directory", str(runtime / "input"), "--output-directory", str(runtime / "output"), "--user-directory", str(runtime / "user"), "--temp-directory", str(runtime / "temp"), "--models-directory", str(runtime / "models"), "--cpu", "--disable-dynamic-vram", "--disable-all-custom-nodes", "--disable-api-nodes", "--disable-auto-launch", "--cache-none", "--database-url", f"sqlite:///{runtime / 'user/comfyui.db'}"]
    os.chdir(output)
    policy = audit_guard(source, fixtures, output)
    report = {"contract": "lantai.synthetic-comfy-run/v1", "harness_sha256": digest(Path(__file__)), "fixture_manifest_sha256": digest(fixtures / "manifest.json"), "mode": "actual-in-process-validator-and-executor-no-server", "network_listener": False, "models_loaded": False, "custom_nodes_loaded": False, "policy": policy, "versions": []}
    try:
        import comfy.options
        comfy.options.enable_args_parsing()
        import folder_paths
        import nodes
        import execution
        import comfyui_version
        import torch
        import numpy as np
        from PIL import Image

        if any(not Path(path).resolve().is_relative_to(output) for paths, _ in folder_paths.folder_names_and_paths.values() for path in paths):
            raise ValueError("ComfyUI model/custom-node directories escaped synthetic evidence")
        report.update({"comfyui_version": comfyui_version.__version__, "torch_version": torch.__version__, "python_version": sys.version.split()[0], "validator_sha256": digest(source / "execution.py"), "nodes_sha256": digest(source / "nodes.py")})
        for asset in recipes:
            label = asset["version_label"]
            asset_root = fixtures / asset["root"]
            workspace = output / label
            folder_paths.set_input_directory(str(workspace / "input"))
            folder_paths.set_output_directory(str(workspace / "output"))
            folder_paths.set_user_directory(str(workspace / "user"))
            folder_paths.set_temp_directory(str(workspace / "temp"))
            api = json.loads((asset_root / "workflow-api.json").read_text())
            ui = json.loads((asset_root / "workflow-ui.json").read_text())
            if {node["class_type"] for node in api.values()} != {"LoadImage", "ImageScale", "SaveImage"}:
                raise ValueError("only the three no-model synthetic nodes are allowed")
            validation = asyncio.run(execution.validate_prompt(f"synthetic-{label}", api, None))
            if not validation[0] or validation[3]:
                raise ValueError(f"official prompt validation failed: {validation}")
            sink = EventSink()
            executor = execution.PromptExecutor(sink, cache_type=execution.CacheType.NONE, cache_args={"ram": 0, "ram_inactive": 0})
            executor.execute(api, f"synthetic-{label}", extra_data={"extra_pnginfo": {"workflow": ui}}, execute_outputs=validation[2])
            if not executor.success or not any(event == "execution_success" for event, _ in executor.status_messages):
                raise ValueError(f"official executor failed: {executor.status_messages}")
            saved = list((workspace / "output").glob("*.png"))
            if len(saved) != 1:
                raise ValueError("expected exactly one generated image")
            with Image.open(saved[0]) as actual, Image.open(workspace / "input/synthetic.png") as original:
                width, height = api["2"]["inputs"]["width"], api["2"]["inputs"]["height"]
                if actual.size != (width, height) or json.loads(actual.info["workflow"]) != ui or json.loads(actual.info["prompt"]) != api:
                    raise ValueError("output dimensions or embedded workflow identity mismatch")
                expected = np.asarray(original.convert("RGB").resize((width, height), Image.Resampling.NEAREST)).astype(np.int16)
                difference = int(np.abs(np.asarray(actual.convert("RGB")).astype(np.int16) - expected).max())
                if difference > 1:
                    raise ValueError("real node output differs from known nearest-neighbor result")
            report["versions"].append({"version_label": label, "status": "passed", "prompt_validation": "passed", "executed_outputs": validation[2], "output": saved[0].relative_to(output).as_posix(), "output_sha256": digest(saved[0]), "dimensions": [width, height], "max_channel_delta": difference, "workflow_metadata_matches": True, "status_messages": executor.status_messages})
        report["status"] = "passed"
    except Exception as error:
        report["status"] = "blocked"
        report["error"] = f"{type(error).__name__}: {error}"
        raise
    finally:
        (output / "validation.json").write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n")
    print(json.dumps({"status": report["status"], "comfyui_version": report["comfyui_version"], "versions": len(report["versions"]), "report": str(output / "validation.json")}, ensure_ascii=False))


if __name__ == "__main__":
    main()
