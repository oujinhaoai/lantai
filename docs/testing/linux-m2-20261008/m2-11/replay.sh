#!/usr/bin/env bash
# Only two independent cloud tests. Does not run the general CI suite.
set -uo pipefail
repo_dir=${1:?Usage: replay.sh REPOSITORY NEW_EVIDENCE_DIRECTORY [chain|repro|both]}
evidence_dir=${2:?Supply a new evidence directory}
test_mode=${3:-both}
cd "$repo_dir" || exit 2
expected_commit=bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7
observed_commit=$(git rev-parse HEAD) || exit 2
if [[ "$observed_commit" != "$expected_commit" ]]; then
  printf 'Refusing unrecorded baseline %s; expected %s\n' "$observed_commit" "$expected_commit" >&2
  exit 2
fi
git diff --exit-code --quiet || { printf 'Tracked worktree modifications detected\n' >&2; exit 2; }
if [[ -e "$evidence_dir" ]]; then
  printf 'Use a new evidence directory to retain original attempts\n' >&2
  exit 2
fi
mkdir -p "$evidence_dir" || exit 2
evidence_dir=$(cd "$evidence_dir" && pwd) || exit 2
case "$test_mode" in
  chain) pattern='^TestCloudM211LinuxPushRestore$' ;;
  repro) pattern='^TestCloudM211MCPPushSchemaRepro$' ;;
  both) pattern='^TestCloudM211(LinuxPushRestore|MCPPushSchemaRepro)$' ;;
  *) printf 'Unknown mode %s\n' "$test_mode" >&2; exit 2 ;;
esac
# The managed cloud worktree fails Go VCS stamping in spawned CLI builds.
# HEAD, tree, exact sources and build SHA-256 are recorded independently.
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
export LANTAI_M211_EVIDENCE="$evidence_dir"
git rev-parse HEAD 'HEAD^{tree}' > "$evidence_dir/baseline.txt"
go version > "$evidence_dir/go-version.txt"
python3 --version > "$evidence_dir/python-version.txt"
cp tests/integration/cloud_m211_linux_test.go tests/integration/cloud_m211_mcp_schema_repro_test.go "$evidence_dir/"
go test -race -count=1 -timeout=10m -json -run "$pattern" ./tests/integration > "$evidence_dir/go-test.jsonl" 2> "$evidence_dir/stderr.log"
test_result=$?
printf '%s\n' "$test_result" > "$evidence_dir/exit-code.txt"
printf 'Exit %s; evidence: %s\n' "$test_result" "$evidence_dir"
exit "$test_result"
