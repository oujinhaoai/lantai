# 精确提交的发布制品

`release-artifacts` 是维护者手动运行的 GitHub Actions 工作流。它生成可导入的 Linux amd64 核心/Caddy 镜像、六平台客户端二进制、独立 Web/help 静态包和部署配置包，不发布 registry、GitHub Release 或 Git tag，不部署目标 NAS、不创建真实管理员或生产凭据；验收仅建立随机隔离的合成实例。版本由维护者明确选择；例如 `v0.1.0-rc.1` 是候选版，MCP 的固定 implementation 版本不决定核心版本。实际阶段验收仍按独立证据判断。

## 前置与触发

先将必要源码和本工作流合入 main，再等待目标提交的 `ci.yml` main push 运行全部 10 项成功。必须使用 40 位精确提交 SHA；源树必须干净、Go 必须是 `go.mod` 固定的 1.26.9，并含完整 Docker 输入（包括 `api/`）。工作流位于默认分支才可手动触发，依据 [GitHub 文档](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)。

```sh
gh workflow run release-artifacts.yml --ref main \
  -f commit=FINAL_MAIN_40_CHARACTER_SHA -f version=v0.1.0-rc.5 \
  -f baseline_run=38394530667 \
  -f baseline_artifact=lantai-v0.1.0-rc.4-linux-amd64-d005b2ddc9911547365d7a5925cb59a8511efdbb \
  -f baseline_commit=d005b2ddc9911547365d7a5925cb59a8511efdbb
```

控制脚本和目标源码各自 checkout，目标必须在 origin/main 历史上并具有该精确提交的成功 main CI。检查缺失、跳过、失败或名称不符都拒绝构建。工作流仅授予 contents/actions 读取权限，无额外 secret；版本参数经格式校验后通过 argv 传给命令，不作为 shell 代码。源码、制品和实际客户端验收应绑定同一明确目标；旧提交通过不能自动沿用。

在本机 Linux Docker/Compose 构建机也可运行同一脚本，需要 Python ≥ 3.11、Git、Go 1.26.9、Node 24/npm、锁定的 Chromium 和 gh；必须使用本地 Linux Unix Docker socket（拒绝远程 context/DOCKER_HOST），输出位于源码树外的新目录：

```sh
# gh 使用构建者已授权的只读身份；先 fetch main，保留工作区已有改动。
git fetch origin main
export GITHUB_REPOSITORY=OWNER/REPOSITORY
python3 scripts/build-release.py --source /path/to/clean-checkout \
  --commit FINAL_MAIN_40_CHARACTER_SHA --version v0.1.0-rc.1 \
  --out /path/outside-source/new-artifacts --require-main-ci \
  --upgrade-from-dir /path/to/verified-original-release-artifacts \
  --upgrade-from-commit ORIGINAL_RELEASE_40_CHARACTER_SHA
```

省略 `--require-main-ci` 仅适合已有独立门禁证据的本机开发构建，此时 manifest.main_ci 为 null，不能称为经 main CI 门禁的发行。脚本拒绝修改源码和覆盖已有输出，并从精确提交新建本地 clone/checkout，仅复制提交对象；忽略文件与未跟踪文件不会进入 Docker 上下文或 Go 构建。工作流的 Docker 构建不是 Go 编译模拟；失败不上传成功制品目录。

## 制品与验证

Actions artifact 保存 30 天。名称含版本、目标提交和 Linux amd64 镜像平台；其中包含：

- 核心镜像与官方 Caddy 2.11.4 的 `*-image.tar.gz`，可解压后经 `docker load` 或宿主支持界面导入。核心 tag 含版本和提交前 12 位，Caddy tag 含 image ID，避免使用可变旧测试 tag。
- Linux、macOS、Windows 的 amd64/arm64 二进制 `lantai-版本-系统-架构.tar.gz` 及 LICENSE；Windows 文件名为 lantai.exe。均为 CGO=0、trimpath 构建，buildinfo 必须包含精确 clean VCS revision、Go 和平台。交叉编译不代表六平台运行验收。
- 精确 `git archive` 源码包、有效 Dockerfile 与专用 allowlist、工具版本、image inspect、版本输出、各平台 buildinfo、`release-images.env`、manifest.json 与 SHA256SUMS。

脚本解析 Go 基础镜像的 repository digest，再用仓库外 Dockerfile 固定该 digest；保留原源码与 Docker allowlist。RepoDigest 可能指向多平台索引，因此另记录实测 linux/amd64 image ID。核心 image 的 OCI revision/version 和 UID:GID 65532:65532 必须匹配。短命、无网络容器运行真实 `version --json`，核对 Go 和平台；独立 Linux amd64 二进制另运行 version 核对 VCS。Docker 上下文没有 .git，不能假设镜像内和独立二进制的 SHA 或 VCS 信息相同；由源归档、有效构建输入、OCI label、image ID 和归档摘要共同绑定。

从导出的 Caddy 镜像复制其二进制，运行现有 `TestCaddySingleHTTPSGateway` 的真实进程、合成 loopback 流式/取消/路由验证；skip 不计通过。

随后用 `scripts/test-compose-first-start.py --image <核心 tag> --caddy-image <网关 tag>` 验证这两份本地发行镜像，不重建或拉取替代镜像。随机测试项目仅使用合成证书、引导码和数据，检查单次 Compose up、TLS、受保护管理员设置、重启不重开引导及日志隐私；实际运行的两个容器 image ID 必须与归档对应清单一致。证据写入 `first-start-image-smoke.txt` 并计入 manifest 与 SHA256SUMS，缺少成功或精确 image ID 证据不生成成功清单。真实 NAS 的文件管理权限、文件系统和流量仍需单独验收。

首次启动验收另保存 `first-start-lifecycle.json`：在同一已设置实例上执行 core 单独重启及三轮完整 stop/up，记录各阶段 core/gateway/prepare 的状态、退出码、实际镜像、配置与实际 HTTPS 端口映射，以及宿主端口、容器端口和 gateway→core 的连接结果。每轮完整重启的 meta 恢复判定以 60 秒为限，截止时间固定在 `up -d` 返回时，同步诊断和请求耗时计入同一窗口，HTTP 响应读完后再次检查截止时间，截止后返回 200 也判失败。单次 socket 超时取剩余预算与 5 秒中的较小值，轮询间隔也不超过剩余预算；全部恢复必须重新通过 TLS 的 meta=200 和 setup=404。诊断采用字段白名单，不输出环境变量、挂载内容、认证请求/响应或设置后的容器日志。失败时保留该 JSON 和非零退出，不能生成成功 manifest；诊断缺项不能代替实际恢复验收。

官方 Caddy 二进制带 `cap_net_bind_service` 文件能力。版本探测使用无网络、只读容器，先丢弃全部能力，再仅保留 `NET_BIND_SERVICE`；不授予 privileged 或宿主网络。该工作流的独立 Docker 回归会读取文件能力，复现全部丢弃时的退出126，并验证单一能力配置能实际执行版本命令。相关发布工具变更的 PR 自动运行此回归，PR 同时构建独立候选制品；手动 main 构建还要求精确 main CI 门禁。命令失败保留非零退出并输出捕获的 stderr，不能把失败探测当作成功。

下载完整 artifact 解压到独立目录后，先运行 `sha256sum -c SHA256SUMS`（macOS 用 `shasum -a 256 -c SHA256SUMS`），核对 manifest.source_commit/main_ci。导入镜像后核对 image inspect 的 ID、平台和 label 与清单一致；不要仅凭 tag 相同判断。归档与摘要没有自动签名，来源应绑定授权的 Actions run/提交和可信交付渠道。

## 部署边界

私有部署配置、TLS 私钥与数据放在仓库/制品之外。镜像导入不等于运行验收；本流程不启动目标服务、不改变系统信任。私有 CA 的显式客户端信任见[客户端契约](contracts/client.md)，部署见[单网关模板](deployment.md)。设备故障、恢复/RPO、实际客户端和长稳验收均按其独立任务执行，不因制品构建成功自动收口。

首次使用 rc2 的人可直接读[管理员设置逐步指南](first-setup.md)。本地交付包若只修订文档，应保留旧文档/ZIP 与校验索引，标明文档修订日期，刷新文档及 ZIP 的摘要；镜像、运行配置、原发行清单与其 `source_commit` 保持不变，不把文档提交冒充镜像重建或新的 Release。

## 网页与直接升级的发行验收

PR 的发行工具回归也构建实际镜像和静态包，版本为 v0.0.0-pr.N，main_ci 为 null；它不发布、不等于正式 main 制品。手动 main 构建仍要求精确源码的十项成功 CI。基线参数选择原发行的 Actions artifact，不读取生产目录；artifact 过期或无法验证时失败停止，由维护者提供核验后的原发行归档，不跳过直接升级证明。

在干净 committed-source clone 中运行帮助生成/check→npm ci→完整 lint→Vite；Node/npm/lockfile 摘要随 manifest 记录。仅白名单普通静态文件进入 `*-web.tar.gz` 的 web/，部署配置进入 `*-deployment.tar.gz` 的 deploy/，包括 compose.backup.yaml、含可选备份变量的 env.example 和 online-backup.md。全部文件均进入 deployment.files 的摘要清单，归档本身进入 manifest.files 和 SHA256SUMS；无 Node 服务、运行数据、真实私钥/令牌或系统信任变更。

`test-release-delivery.py` 重新加载导出的实际镜像并解包刚生成的静态/配置归档，核对全部成员与摘要，通过真实 Caddy 返回逐文件同字节内容。两个独立合成 Docker 项目分别验证新装及原 rc.4→目标版直接升级：从 rc.4 自身的网页/配置包和镜像建立身份/资源与非默认逻辑卷名，再保留项目、端口、data/key/setup/config 挂载、实例及原管理员/TOTP 切目标；正常登录、Cookie/CSRF、授权预览/下载、帮助原生返回、无JS帮助和API/运维隔离均来自实际发行服务，无 mock 或 dev server。测试 TLS 私钥与身份仅在私有临时目录，证据只记录非秘密对比及登录后截图；Chromium 的测试例外精确绑定该次合成证书 SPKI，Python 独立验证 CA/主机名，不修改生产或系统信任。

发行 manifest 只有上述验收成功后才生成，绑定静态包/config包的版本、source_commit、逐文件hash与实际运行核心/Caddy image ID。失败/skip不能当成功。代码/静态包/真实 NAS 仍需分别归因；条件反向回退只按本次实际测试版本对及未迁移状态认定。用户从[新装](../deploy/release-install.md)或[原地升级](../deploy/release-upgrade.md)选择正确入口，已有实例不能拿新装 Compose/.env 覆盖原项目。

升级比较只允许精确镜像引用、既有静态目录来源、备份根两项环境和一个 bind、以及 /tmp 的 mode=1777 修补；项目、卷定义与来源、TLS、端口、其他环境和安全限制改变即失败。新装与升级后均通过同一运行 core 执行 backup-online，另用仅挂备份目录、无网络、只读且非 root 的目标镜像运行 backup-verify；不向核验容器挂密钥。回退恢复原 rc.4 的完整配置和原网页目录，不能移除旧网页挂载或重新初始化。缺 backup_smoke_passed 或 rollback_passed 的结果不生成成功 manifest。
