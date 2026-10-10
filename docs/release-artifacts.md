# 精确提交的发布制品

`release-artifacts` 是维护者手动运行的 GitHub Actions 工作流。它生成可导入的 Linux amd64 核心/Caddy 镜像和六平台客户端二进制，不发布 registry、GitHub Release 或 Git tag，不部署服务、不创建管理员或生产凭据。版本由维护者明确选择；例如 `v0.1.0-rc.1` 是候选版，MCP 的固定 implementation 版本不决定核心版本。实际阶段验收仍按独立证据判断。

## 前置与触发

先将必要源码和本工作流合入 main，再等待目标提交的 `ci.yml` main push 运行全部 10 项成功。必须使用 40 位精确提交 SHA；源树必须干净、Go 必须是 `go.mod` 固定的 1.26.9，并含完整 Docker 输入（包括 `api/`）。工作流位于默认分支才可手动触发，依据 [GitHub 文档](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)。

```sh
gh workflow run release-artifacts.yml --ref main \
  -f commit=FINAL_MAIN_40_CHARACTER_SHA -f version=v0.1.0-rc.1
```

控制脚本和目标源码各自 checkout，目标必须在 origin/main 历史上并具有该精确提交的成功 main CI。检查缺失、跳过、失败或名称不符都拒绝构建。工作流仅授予 contents/actions 读取权限，无额外 secret；版本参数经格式校验后通过 argv 传给命令，不作为 shell 代码。源码、制品和实际客户端验收应绑定同一明确目标；旧提交通过不能自动沿用。

在已有受支持的 Docker/Compose 构建机也可运行同一脚本，需要 Python ≥ 3.11、Git、Go 1.26.9 和 gh；输出必须位于源码树外的新目录：

```sh
# gh 使用构建者已授权的只读身份；先 fetch main，保留工作区已有改动。
git fetch origin main
export GITHUB_REPOSITORY=OWNER/REPOSITORY
python3 scripts/build-release.py --source /path/to/clean-checkout \
  --commit FINAL_MAIN_40_CHARACTER_SHA --version v0.1.0-rc.1 \
  --out /path/outside-source/new-artifacts --require-main-ci
```

省略 `--require-main-ci` 仅适合已有独立门禁证据的本机开发构建，此时 manifest.main_ci 为 null，不能称为经 main CI 门禁的发行。脚本拒绝修改源码和覆盖已有输出，并从精确提交新建本地 clone/checkout，仅复制提交对象；忽略文件与未跟踪文件不会进入 Docker 上下文或 Go 构建。工作流的 Docker 构建不是 Go 编译模拟；失败不上传成功制品目录。

## 制品与验证

Actions artifact 保存 30 天。名称含版本、目标提交和 Linux amd64 镜像平台；其中包含：

- 核心镜像与官方 Caddy 2.11.4 的 `*-image.tar.gz`，可解压后经 `docker load` 或宿主支持界面导入。核心 tag 含版本和提交前 12 位，Caddy tag 含 image ID，避免使用可变旧测试 tag。
- Linux、macOS、Windows 的 amd64/arm64 二进制 `lantai-版本-系统-架构.tar.gz` 及 LICENSE；Windows 文件名为 lantai.exe。均为 CGO=0、trimpath 构建，buildinfo 必须包含精确 clean VCS revision、Go 和平台。交叉编译不代表六平台运行验收。
- 精确 `git archive` 源码包、有效 Dockerfile 与专用 allowlist、工具版本、image inspect、版本输出、各平台 buildinfo、`release-images.env`、manifest.json 与 SHA256SUMS。

脚本解析 Go 基础镜像的 repository digest，再用仓库外 Dockerfile 固定该 digest；保留原源码与 Docker allowlist。RepoDigest 可能指向多平台索引，因此另记录实测 linux/amd64 image ID。核心 image 的 OCI revision/version 和 UID:GID 65532:65532 必须匹配。短命、无网络容器运行真实 `version --json`，核对 Go 和平台；独立 Linux amd64 二进制另运行 version 核对 VCS。Docker 上下文没有 .git，不能假设镜像内和独立二进制的 SHA 或 VCS 信息相同；由源归档、有效构建输入、OCI label、image ID 和归档摘要共同绑定。

从导出的 Caddy 镜像复制其二进制，运行现有 `TestCaddySingleHTTPSGateway` 的真实进程、合成 loopback 流式/取消/路由验证；skip 不计通过。它仍是网关组件测试，不能代替目标主机的 core 初始化、真实容器卷权限或 NAS 流量验收。

官方 Caddy 二进制带 `cap_net_bind_service` 文件能力。版本探测使用无网络、只读容器，先丢弃全部能力，再仅保留 `NET_BIND_SERVICE`；不授予 privileged 或宿主网络。该工作流的独立 Docker 回归会读取文件能力，复现全部丢弃时的退出126，并验证单一能力配置能实际执行版本命令。相关发布工具变更的 PR 自动运行此回归，PR 中的正式制品任务仍跳过；手动 main 发行同时运行回归与构建。命令失败保留非零退出并输出捕获的 stderr，不能把失败探测当作成功。

下载完整 artifact 解压到独立目录后，先运行 `sha256sum -c SHA256SUMS`（macOS 用 `shasum -a 256 -c SHA256SUMS`），核对 manifest.source_commit/main_ci。导入镜像后核对 image inspect 的 ID、平台和 label 与清单一致；不要仅凭 tag 相同判断。归档与摘要没有自动签名，来源应绑定授权的 Actions run/提交和可信交付渠道。

## 部署边界

私有部署配置、TLS 私钥与数据放在仓库/制品之外。镜像导入不等于运行验收；本流程不启动目标服务、不改变系统信任。私有 CA 的显式客户端信任见[客户端契约](contracts/client.md)，部署见[单网关模板](deployment.md)。设备故障、恢复/RPO、实际客户端和长稳验收均按其独立任务执行，不因制品构建成功自动收口。
