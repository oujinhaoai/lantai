#!/usr/bin/env bash
# 本地与 CI 共用的检查：格式、go vet、staticcheck、依赖整洁、生成物无漂移、测试。
#   LANTAI_RACE=0 scripts/check.sh   # 跳过 -race（没有 C 工具链的环境）
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n==> %s\n' "$*"; }

step "gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "需要 gofmt 的文件："
  echo "$unformatted"
  exit 1
fi

step "go mod tidy（主模块与工具模块）"
go mod tidy -diff
(cd scripts/tools && go mod tidy -diff)

step "go vet"
go vet ./...

step "staticcheck"
go tool -modfile=scripts/tools/go.mod staticcheck ./...

step "生成物无漂移"
go run ./scripts/gen/errcodes -check
go run ./scripts/gen/ownership -check
go run ./scripts/gen/identity -check
go run ./scripts/gen/openapi -check
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
sed "s#^output: .*#output: $tmp/models.gen.go#" api/oapi-codegen.yaml > "$tmp/oapi-codegen.yaml"
go tool -modfile=scripts/tools/go.mod oapi-codegen -config "$tmp/oapi-codegen.yaml" api/gen/openapi.bundle.json
if ! cmp -s internal/apiv1/models.gen.go "$tmp/models.gen.go"; then
  echo "internal/apiv1/models.gen.go 与生成结果不一致；运行 scripts/generate.sh"
  diff -u internal/apiv1/models.gen.go "$tmp/models.gen.go" || true
  exit 1
fi

step "Python SDK 契约与测试"
python3 sdk/python/generate.py --check
PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v

# 与三平台 CI 一致：20 分钟限制整个包的累计测试，避免真实实例/强杀
# 矩阵增长后触及 Go 默认的 10 分钟；不改变单个用例断言或性能阈值。
step "go test"
if [ "${LANTAI_RACE:-1}" = "1" ]; then
  go test -timeout=20m -race -count=1 ./...
else
  go test -timeout=20m -count=1 ./...
fi

step "构建 lantai"
go build -o /dev/null ./cmd/lantai

echo
echo "全部检查通过"
