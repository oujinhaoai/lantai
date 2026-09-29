# 最小 Python 客户端

Python ≥ 3.11，运行时只用标准库。包版本为 0.2.0，公共 API 版本为 `v1`；`Client.meta()` 拒绝其他 API 版本。此包调用同一 REST，不复制权限判定、不访问服务端数据库或规范目录，也不提供插件宿主。尚未向 PyPI 发布。

```sh
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python
# 或不安装，直接使用工作树：
PYTHONPATH=sdk/python python3 sdk/python/examples/read_task.py
```

示例从 `LANTAI_SERVER`、`LANTAI_SESSION_TOKEN`、`LANTAI_PROJECT_ID` 读取已有会话和合成测试项目，不把凭据写入源码、参数或输出。服务端必须是 HTTPS origin；仅本机开发可显式 `Client("http://127.0.0.1:8080", allow_http=True)`。TLS 验证始终开启，重定向和跨 origin 的文件授权被拒绝。

```python
from lantai import APIError, Client

client = Client("https://gateway.example", session_token="SESSION_FROM_PRIVATE_CONFIG")
client.meta()
client.whoami()
page = client.tasks("01ARZ3NDEKTSV4RRFFQ69G5FAV", limit=20)
# tasks 使用 after=上一页最后一个 task 的 id；assets search 使用返回的 cursor。
```

主要方法：`exchange`（已有长期 token 换会话）、`whoami`、`search`、`version`、`create_upload`、`commit`、`tasks`、`task`、`create_task`、`task_command`、`operation` 和 `download_file`。更细端点使用 `request(method, path, body, idempotency_key=..., if_match=...)`，只允许生成契约中的方法/路径。缺能力由服务端明确拒绝。创建上传/提交是低层封装；完整分片上传和断点工作副本使用 CLI push/pull，SDK 不暗中重放写请求。

`APIError` 保留服务端 `code`、`retryable`、`recovery_action`、`operation_id` 和完整非秘密错误字段；调用方按机器字段分支。认证材料不出现在异常文本。非 JSON、重定向或错误封装不合法产生 `ProtocolError`，网络失败产生 `TransportError`。每次底层 I/O 超时默认 30 秒，JSON 上限 8 MiB；不跟随系统 HTTP 代理。

`download_file(asset_id, version_id, path, directory)` 取得精确 manifest 和新的 read grant，以 1 MiB 缓冲流式下载，核验 size/SHA-256，最后以同目录硬链接原子安装且不覆盖。路径必须相对于指定现有目录，禁止穿越和符号链接父目录；底层必须支持硬链接。它不续传单文件，失败时清理自己的临时文件；大规模可恢复搬运用 CLI。工作目录须由调用方控制，路径约束不构成不可信本机进程的隔离沙箱。

## 生成与验证

- 手写：`lantai/client.py`（HTTP/认证/便捷方法），`__init__.py`、测试与示例。
- 生成：`lantai/_contract.py`，只含 `api/gen/openapi.bundle.json` 的 SHA-256、API 版本和公开路由白名单；不生成第二份领域协议或权限表。
- 权威：仓库 `api/openapi.yaml`、`schemas/`。先 `scripts/generate.sh`；单独重复 `python3 sdk/python/generate.py` 结果相同，`--check` 检查漂移。
- `PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v` 验证认证、机器错误、同源/路径与精确下载。
- `go test ./tests/integration -run TestRemoteM2 -count=1` 用同一个真实 Go 服务验证 REST、CLI、官方 MCP 和 Python 的语义。开发测试不代替独立 TEST-M2-11。

构建使用 setuptools ≥ 68（MIT），仅作为打包工具，无 Python 运行依赖。本包沿用仓库 GPL-3.0，许可证随包分发。
