#!/usr/bin/env python3
"""Linux M2-10 owner-only process test. Synthetic runtime, no services/credentials.

Install only the adjacent private test file in an isolated baseline checkout;
never reset/clean/commit/push, and never alter existing product or fixture files.
Every invocation requires a new output directory, preserving earlier failures.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import sqlite3
import subprocess
import sys
import time
import traceback

BASE = "bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7"
PHASES = [
    ("seed", "primary", True), ("hold", "primary", True),
    ("reconcile", "primary", True), ("recover", "primary", False),
    ("closed", "primary", False), ("window_cold", "primary", False),
    ("fault_seed", "fault", True), ("fault_recover", "fault", False),
    ("fault_closed", "fault", False),
]
SOURCES = [
    "AGENTS.md", "README.md", "go.mod", "go.sum", "docs/extensions.md",
    "docs/development.md", "docs/architecture.md", "docs/tasks/T09-extension-platform.md",
    "docs/testing/commit-fault-matrix.md", "internal/extensions/breaker.go",
    "internal/extensions/breaker_test.go", "internal/contract/execution/states.go",
    "internal/contract/clock/clock.go", "internal/platform/sqlite/sqlite.go",
    "internal/platform/sqlite/gate.go", "internal/platform/sqlite/migrations/migrations.go",
    "internal/platform/sqlite/migrations/sql/runtime/0013.extensions.runtime.sql",
]


def utc():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="microseconds")


def sha(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def write_json(path, data):
    with Path(path).open("x", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())


def git(source, *args):
    return subprocess.check_output(["git", "-C", str(source), *args], text=True).strip()


def read_db(db):
    # mode=ro plus query_only; never checkpoint or repair the evidence DB.
    conn = sqlite3.connect(db.resolve().as_uri() + "?mode=ro", uri=True, timeout=5)
    try:
        conn.execute("PRAGMA query_only=ON")
        raw = conn.execute("SELECT record FROM extensions_breakers ORDER BY breaker_key").fetchall()
        if len(raw) != 1:
            raise RuntimeError(f"expected one breaker, found {len(raw)}")
        inv = conn.execute("SELECT record FROM extensions_invocations ORDER BY invocation_id").fetchall()
        faults = conn.execute("SELECT breaker_key,invocation_id,epoch,at FROM extensions_breaker_faults ORDER BY invocation_id").fetchall()
        integrity = [r[0] for r in conn.execute("PRAGMA integrity_check")]
        result = {
            "wall_utc": utc(), "reader_pid": os.getpid(),
            "reader_sqlite_version": sqlite3.sqlite_version,
            "read_only": True, "integrity_check": integrity,
            "persisted": json.loads(raw[0][0]),
            "invocations": [json.loads(r[0]) for r in inv],
            "faults": [dict(zip(("breaker_key", "invocation_id", "epoch", "at"), r)) for r in faults],
        }
        if integrity != ["ok"]:
            raise RuntimeError(f"integrity failure: {integrity}")
        return result
    finally:
        conn.close()


def compare_committed(marker, independent):
    for key in ("persisted", "invocations", "faults"):
        if marker[key] != independent[key]:
            raise RuntimeError(f"independent reader differs from durable marker: {key}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True, help="isolated checkout/worktree")
    parser.add_argument("--output", type=Path, required=True, help="must not exist")
    parser.add_argument("--go", default="go")
    parser.add_argument("--expected-head", default=BASE,
                        help="default published baseline; change explicitly for future impact regression")
    args = parser.parse_args()
    if sys.platform != "linux":
        parser.error("native Linux required")
    source, out = args.source.resolve(), args.output.resolve()
    if out.is_relative_to(source):
        parser.error("runtime/evidence must be outside repository")
    if git(source, "rev-parse", "HEAD") != args.expected_head:
        parser.error("HEAD differs from expected baseline")
    if git(source, "diff", "HEAD", "--"):
        parser.error("tracked changes found; use a clean isolated worktree")
    out.mkdir(parents=True, exist_ok=False)
    os.chmod(out, 0o700)
    started = utc()
    commands, processes = [], []
    current = None

    def timeline(event, **fields):
        data = {"event": event, "wall_utc": utc(), "monotonic_ns": time.monotonic_ns(),
                "controller_pid": os.getpid(), **fields}
        with (out / "timeline.jsonl").open("a", encoding="utf-8") as f:
            f.write(json.dumps(data, ensure_ascii=False) + "\n")
            f.flush()
            os.fsync(f.fileno())

    def run(name, argv):
        step = {"name": name, "argv": list(map(str, argv)), "cwd": str(source), "started_utc": utc()}
        commands.append(step)
        tick = time.monotonic()
        with (out / (name + ".stdout.log")).open("xb") as stdout, (out / (name + ".stderr.log")).open("xb") as stderr:
            p = subprocess.run(step["argv"], cwd=source, stdout=stdout, stderr=stderr, timeout=180)
        step.update(returncode=p.returncode, elapsed_seconds=time.monotonic() - tick, finished_utc=utc())
        if p.returncode:
            raise RuntimeError(f"command failed: {name}, see original stdout/stderr")

    try:
        fixture = Path(__file__).with_name("linux_m2_10_owner_test.go.txt")
        target = source / "internal/extensions/linux_m2_10_owner_test.go"
        if target.exists() and sha(target) != sha(fixture):
            raise RuntimeError("existing private driver differs; refusing overwrite")
        if not target.exists():
            shutil.copyfile(fixture, target)
        srcdir = out / "source"
        manifest = {}
        for name in SOURCES:
            src, dst = source / name, srcdir / name
            if not src.is_file():
                raise RuntimeError(f"required source absent: {name}")
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(src, dst)
            manifest[name] = sha(src)
        driverdir = out / "driver"
        driverdir.mkdir()
        for src in (fixture, Path(__file__).resolve()):
            shutil.copyfile(src, driverdir / src.name)
        env = {
            "started_utc": started, "controller_pid": os.getpid(),
            "baseline": args.expected_head, "tree": git(source, "rev-parse", "HEAD^{tree}"),
            "source": str(source), "git_status_before": git(source, "status", "--porcelain=v1"),
            "platform": platform.platform(), "uname": list(platform.uname()),
            "python": sys.version, "python_sqlite_version": sqlite3.sqlite_version,
            "go_version": subprocess.check_output([args.go, "version"], cwd=source, text=True).strip(),
            "go_env": json.loads(subprocess.check_output([args.go, "env", "-json", "GOOS", "GOARCH", "GOVERSION", "GOTOOLCHAIN", "CGO_ENABLED", "GOFLAGS", "GOMODCACHE", "GOCACHE"], cwd=source, text=True)),
            "filesystem_type": subprocess.check_output(["stat", "-f", "-c", "%T", str(out)], text=True).strip(),
            "mount": subprocess.check_output(["findmnt", "-T", str(out), "-J", "-o", "TARGET,SOURCE,FSTYPE,OPTIONS"], text=True),
            "source_sha256": manifest, "driver_sha256": sha(fixture),
            "replay_sha256": sha(Path(__file__).resolve()),
            "schema": "only published runtime/0013.extensions.runtime; no full instance, activation records, jobs or registry",
            "logical_clock": "contract clock.Fake, base 2026-10-08T00:00:00Z; no 15-minute real-time observation",
            "host_observations": "synthetic; caller authorization assumed; only breaker owner SIGKILL is real",
        }
        write_json(out / "environment.json", env)
        binary = out / "extensions-owner.test"
        run("compile", [args.go, "test", "-c", "-o", binary, "./internal/extensions"])
        run("baseline-breaker-tests", [binary, "-test.run=^TestBreaker(WindowCountingAndExclusions|CooldownHalfOpenAndEpochs|PolicyOverride)$", "-test.v", "-test.timeout=45s"])
        for phase, branch, kill in PHASES:
            phase_dir = out / "phases" / phase
            phase_dir.mkdir(parents=True)
            runtime = out / "runtime" / branch
            runtime.mkdir(parents=True, exist_ok=True)
            db = runtime / "runtime.db"
            argv = [str(binary), "-test.run=^TestLinuxM210OwnerChild$", "-test.v", "-test.timeout=45s"]
            childenv = os.environ.copy()
            childenv.update(LANTAI_M2_10_PHASE=phase, LANTAI_M2_10_DB=str(db), LANTAI_M2_10_PHASE_DIR=str(phase_dir))
            stdout_path = phase_dir / "stdout.log"
            with stdout_path.open("xb") as stdout, (phase_dir / "stderr.log").open("xb") as stderr:
                current = subprocess.Popen(argv, cwd=source, env=childenv, stdout=stdout, stderr=stderr)
                pid = current.pid
                tick = time.monotonic()
                timeline("child_start", phase=phase, branch=branch, pid=pid, argv=argv)
                record = {"phase":phase,"branch":branch,"pid":pid,"argv":argv,"started_utc":utc(),"intentional_sigkill":kill}
                processes.append(record)
                deadline = time.monotonic() + 30
                marker_path = phase_dir / "marker.json"
                sentinel = f"LT10_READY {phase} {pid}\n".encode()
                while not (marker_path.exists() and sentinel in stdout_path.read_bytes()):
                    if current.poll() is not None:
                        raise RuntimeError(f"child {phase} exited before fsynced marker: {current.returncode}; see raw logs/events")
                    if time.monotonic() > deadline:
                        raise RuntimeError(f"child {phase} fsynced marker timeout")
                    time.sleep(0.01)
                marker = json.loads(marker_path.read_text())
                if marker["pid"] != pid or marker["phase"] != phase:
                    raise RuntimeError("invalid child marker identity")
                record["marker_observed_utc"] = utc()
                timeline("fsynced_marker_observed", phase=phase, pid=pid, marker_sha256=sha(marker_path), logical_ms=marker["logical_ms"])
                if kill:
                    independent = read_db(db)
                    compare_committed(marker, independent)
                    write_json(phase_dir / "independent-before-kill.json", independent)
                    timeline("independent_committed_read", phase=phase, pid=pid, integrity_check=independent["integrity_check"])
                    record["kill_requested_utc"] = utc()
                    timeline("SIGKILL_requested", phase=phase, pid=pid, signal=signal.SIGKILL)
                    os.kill(pid, signal.SIGKILL)
                rc = current.wait(timeout=15)
                record.update(returncode=rc, finished_utc=utc(), elapsed_seconds=time.monotonic()-tick)
                timeline("child_reaped", phase=phase, pid=pid, returncode=rc)
                current = None
                if rc != (-signal.SIGKILL if kill else 0):
                    raise RuntimeError(f"unexpected child termination {phase}: {rc}")
                if (Path("/proc") / str(pid)).exists():
                    raise RuntimeError(f"reaped PID still present: {pid}")
                record["proc_pid_absent_after_reap"] = True
            # Preserve exact quiescent db/WAL/SHM bytes after termination, before
            # the next product process can perform recovery or checkpointing.
            snapdir = phase_dir / "sqlite-files-after-exit"
            snapdir.mkdir()
            for f in sorted(runtime.glob("runtime.db*")):
                shutil.copyfile(f, snapdir / f.name)
            independent = read_db(db)
            compare_committed(marker, independent)
            write_json(phase_dir / "independent-after-exit.json", independent)
            timeline("independent_after_exit_read", phase=phase, pid=pid, integrity_check=independent["integrity_check"])
        pids = [p["pid"] for p in processes]
        if len(set(pids)) != len(PHASES) or os.getpid() in pids:
            raise RuntimeError("owner processes did not have distinct PIDs")
        if git(source, "diff", "HEAD", "--"):
            raise RuntimeError("tracked source changed during execution")
        rows = []
        for phase, _, _ in PHASES:
            for line in (out / "phases" / phase / "events.jsonl").read_text().splitlines():
                event = json.loads(line)
                if "view" in event:
                    rows.append(event)
        write_json(out / "state-matrix.json", rows)
        with (out / "state-matrix.tsv").open("x") as f:
            f.write("phase\tstage\tpid\tlogical_ms\tview\tpersisted\tepoch\tslots\twindow_faults\thistorical_fault_rows\tintegrity\n")
            for r in rows:
                v, p = r["view"], r["persisted"]
                f.write("\t".join(map(str, (r["phase"],r["stage"],r["pid"],r["logical_ms"],v["state"],p["state"] if p else "absent",v["epoch"],len(v["half_open_probes"]),v["window_faults"],len(r["faults"]),r["integrity_check"]))) + "\n")
        write_json(out / "summary.json", {
            "result":"pass", "started_utc":started,"finished_utc":utc(),
            "baseline":args.expected_head, "test_binary_sha256":sha(binary),
            "processes":processes, "distinct_owner_pids":len(set(pids)),
            "intentional_sigkills":sum(p["intentional_sigkill"] for p in processes),
            "child_integrity_checks":len(rows), "parent_read_only_integrity_checks":len(PHASES)+4,
            "all_integrity_checks_ok":True,
            "git_status_after":git(source,"status","--porcelain=v1"),
            "scope":"Linux breaker owner only; logical clock and synthetic host observations; not TEST-M2-10/GATE acceptance",
        })
        write_json(out / "commands.json", {"commands":commands,"processes":processes})
        (out / "COMPLETE").write_text("pass\n")
        print(json.dumps({"result":"pass","output":str(out),"distinct_owner_pids":len(set(pids)),"sigkills":4,"snapshots":len(rows)}), flush=True)
        return 0
    except BaseException:
        if current is not None and current.poll() is None:
            os.kill(current.pid, signal.SIGKILL)
            current.wait(timeout=15)
        failure = traceback.format_exc()
        (out / "failure.txt").write_text(failure)
        write_json(out / "failed-summary.json", {"result":"fail","started_utc":started,"finished_utc":utc(),"processes":processes,"commands":commands,"failure":failure})
        print(failure, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
