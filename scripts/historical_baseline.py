"""Exact historical upgrade inputs, not a substitute for a complete release bundle."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess

REPOSITORY = "oujinhaoai/lantai"
COMMIT = "d27a06fe0af55e205a19e8f95f0764167e217741"
RUN = "38677996246"
ARTIFACT = "lantai-v0.1.0-rc.5-linux-amd64-" + COMMIT
TAG = "ci-baseline-v0.1.0-rc.5-d27a06f"
SCOPE = "rc5-upgrade-inputs"
MANIFEST_SHA = "c4e34a6618da7956240abe1942dc632deaa2fc03277327e2188de90de12bd4a1"
SUMS_SHA = "de341bdb193b83f7541a27033b8e2b24e8edbb971753ba47449943a7441357f4"
INPUTS = (
    "caddy-2.11.4-linux-amd64-image.tar.gz",
    "lantai-v0.1.0-rc.5-linux-amd64-image.tar.gz",
    "lantai-v0.1.0-rc.5-linux-amd64.tar.gz",
    "lantai-v0.1.0-rc.5-web.tar.gz",
    "lantai-v0.1.0-rc.5-deployment.tar.gz",
)
ASSETS = (*INPUTS, "manifest.json", "SHA256SUMS")


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def regular(path):
    if path.is_symlink() or not path.is_file():
        raise RuntimeError("historical baseline requires regular files")


def verify_subset(directory, commit):
    """Verify all consumed old bytes against the unchanged, independently pinned manifest."""
    directory = Path(directory)
    if commit != COMMIT or directory.is_symlink() or not directory.is_dir():
        raise RuntimeError("historical baseline identity mismatch")
    if {p.name for p in directory.iterdir()} != set(ASSETS):
        raise RuntimeError("historical baseline must contain exactly seven original assets")
    for name in ASSETS:
        regular(directory / name)
    if digest(directory / "manifest.json") != MANIFEST_SHA or digest(directory / "SHA256SUMS") != SUMS_SHA:
        raise RuntimeError("historical baseline original metadata mismatch")
    manifest = json.loads((directory / "manifest.json").read_text())
    build = manifest.get("build_run", {})
    if (manifest.get("source_commit") != COMMIT or manifest.get("version") != "v0.1.0-rc.5"
            or str(build.get("run_id")) != RUN or build.get("repository") != REPOSITORY
            or build.get("controller_commit") != COMMIT):
        raise RuntimeError("historical baseline provenance mismatch")
    entries = {}
    for item in manifest["files"]:
        name = item["name"]
        if Path(name).name != name or name in entries:
            raise RuntimeError("unsafe/duplicate historical manifest member")
        entries[name] = item
    for name in INPUTS:
        item = entries[name]
        path = directory / name
        if path.stat().st_size != item["size_bytes"] or digest(path) != item["sha256"]:
            raise RuntimeError("historical upgrade input differs from original manifest")
    return manifest


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True)


def download(repository, run, artifact, commit, destination):
    """Only an explicit successful listing proving absence/expiry permits fallback."""
    destination = Path(destination)
    if destination.exists():
        raise RuntimeError("baseline destination must be absent")
    exact = (repository, run, artifact, commit) == (REPOSITORY, RUN, ARTIFACT, COMMIT)
    if exact:
        pages = json.loads(gh("api", "--paginate", "--slurp",
                              f"repos/{repository}/actions/runs/{run}/artifacts?per_page=100"))
        if (not isinstance(pages, list) or not pages
                or any(not isinstance(page, dict) or not isinstance(page.get("artifacts"), list) for page in pages)):
            raise RuntimeError("incomplete original artifact listing")
        found = [a for page in pages for a in page["artifacts"] if a["name"] == artifact]
        if len(found) > 1:
            raise RuntimeError("ambiguous original baseline artifact")
        if found and not isinstance(found[0].get("expired"), bool):
            raise RuntimeError("invalid original artifact expiry metadata")
        available = bool(found) and not found[0]["expired"]
        if not available:
            destination.mkdir(parents=True)
            patterns = [arg for name in ASSETS for arg in ("--pattern", name)]
            gh("release", "download", TAG, "--repo", repository, "--dir", str(destination), *patterns)
            verify_subset(destination, commit)
            return SCOPE
    # Custom inputs and all available original artifacts retain the full path.
    # A download/auth/network failure never falls through to another source.
    gh("run", "download", run, "--repo", repository, "--name", artifact, "--dir", str(destination))
    return "full"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("repository", "run", "artifact", "commit", "destination"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    scope = download(args.repository, args.run, args.artifact, args.commit, args.destination)
    with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
        stream.write("scope=" + scope + "\n")


if __name__ == "__main__":
    main()
