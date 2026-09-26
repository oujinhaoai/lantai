#!/usr/bin/env bash
# 重新生成全部派生文件：错误码常量与表、所有权文档、OpenAPI 打包文档与 Go 传输类型。
# 生成器与工具版本分别锁定在 go.mod 与 scripts/tools/go.mod。
set -euo pipefail
cd "$(dirname "$0")/.."

go run ./scripts/gen/errcodes
go run ./scripts/gen/ownership
go run ./scripts/gen/openapi
go tool -modfile=scripts/tools/go.mod oapi-codegen -config api/oapi-codegen.yaml api/gen/openapi.bundle.json
