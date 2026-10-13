# 在 NAS 安装

目标：通过 NAS 图形界面启动一个全新实例。不要求 NAS 提供 `docker compose` 命令；需要它的项目编辑器能够正确执行 Compose 依赖条件。已有资料的实例请走[升级](upgrade.md)，不要套用新安装流程。

## 前提与操作位置

准备与 NAS CPU 架构匹配、已核验 SHA-256 的 Lantai 发行镜像及配置包，已有可信 HTTPS 证书和对应域名，以及 NAS 管理权限。发行物核验见[发行说明](../release-artifacts.md)。源码里的 `deploy/compose.yaml` 是模板，不是任何 NAS 通用的一键安装包。

在 NAS 的**镜像管理**导入镜像，在**项目 / Compose 编辑器**保存配置，在**文件管理器**保存私有配置和证书。不同厂商按钮名称不同，本手册按对象说明，不假定某个固件的菜单。

## 1. 导入并核对镜像

1. 在发行包中找到镜像归档与校验和，先核对归档 SHA-256。不要只凭文件名或 `latest` 标签判断版本。
2. 在 NAS 镜像管理器选择“导入”，导入 Lantai 核心镜像和配置包指定的 Caddy 镜像。导入不会让旧容器自动更新。
3. 记录镜像 tag、image ID、OCI revision 和架构。rc.4 的 source_commit、镜像 ID 和静态包摘要均以同一平台发行 manifest 为准；同时准备 Web/help 与部署配置归档，不能只导入 core 镜像。

成功标志：镜像管理器中存在清单指定的镜像，架构匹配。导入失败或校验不符时重新取得原发行物，不进入启动步骤。

## 2. 准备目录和 Compose 参数

1. 建一个新的私有项目目录。将同版 `*-deployment.tar.gz` 解出 `deploy/`（内含 Compose、Caddyfile 和 config 模板），将 `*-web.tar.gz` 解出 `web/`；逐文件核对 manifest 摘要。另备私有 `setup/` 和 `tls/`，不要放在公共共享目录里。
2. 把证书链和私钥按配置放到 `tls/cert.pem`、`tls/key.pem`。让容器所需身份可读，其他用户不可读私钥；不要放宽成全员可写。
3. 在项目编辑器使用发行的 `deploy/compose.yaml`，将 `compose.web.yaml` 中的只读静态 bind 合入 `services.gateway.volumes`，保留其余挂载。GUI 的主目录指向该 `deploy/` 目录，使 `./Caddyfile` 正确定位；支持多文件的设备也可按顺序加载这两个文件。将 env.example 复制到自己的私有 env 后逐项填写并在 GUI 选择它；若 GUI 不会加载 env，在环境变量编辑器填值，或由部署者明确展开占位值，不留下未解析的 `${…}`。
4. 核对下表所有值和挂载。若端口不是 443，Origin 必须包含端口。示例 `https://library.example:32444` 只用于说明。

| 配置项 | 应填什么 |
| --- | --- |
| `LANTAI_IMAGE` | 已导入且审核过的 Lantai 精确镜像 tag；`prepare` 与 `core` 一致 |
| `LANTAI_CADDY_IMAGE` | 同一发行 manifest 中的 Caddy 精确镜像 tag；保留 `pull_policy: never` |
| `LANTAI_WEB_DIR` | 核验后的 `web/` 绝对目录，直接包含 index.html、assets/、help/；仅 gateway 挂到 `/srv/lantai-web` 且只读 |
| `LANTAI_DOMAIN` / `LANTAI_HTTPS_PORT` | 证书覆盖的域名 / 未占用的 HTTPS 端口 |
| `LANTAI_CONFIG_FILE` | 本项目 `config.container.yaml` 的实际文件路径 |
| `LANTAI_SETUP_DIR` | 本项目私有 `setup/` 的实际目录 |
| `LANTAI_TLS_DIR` | 本项目私有 `tls/` 的实际目录 |
| `http.allowed_origins` | `config.container.yaml` 中精确的 HTTPS origin，例如 `https://library.example:32444` |
| 数据与密钥挂载 | `/var/lib/lantai`、`/var/lib/lantai-secrets` 各自的实际命名卷或受支持本机目录 |

默认 `lantai_data` 和 `lantai_secrets` 会由 Compose 项目名派生实际卷名；**记录实际卷名，不靠项目名猜测**。新实例不得指向旧实例的数据卷或密钥卷。部分 NAS 共享目录通过 FUSE 映射，不适合 SQLite；优先采用已在目标机验证的 Docker 命名卷。原始文件系统检查通过不能替代并发测试，详见[数据位置](../deployment.md#数据位置)。

## 3. 启动与首次设置

1. 在项目界面“启动 / 应用”一次。`prepare` 用 root 仅准备空目录权限，正常退出 0；已有非空目录只校验，不递归更改。
2. 检查 `core`、`gateway` 状态。`core` 使用 UID/GID `65532:65532`。只发布网关的 HTTPS 端口，8080、8081、9090 不对外发布。
3. 在浏览器打开自己的 `https://域名:端口/setup/`。引导码从 `core` 的 `/var/lib/lantai-setup` 实际宿主挂载下 `setup-code.json` 读取，继续按[首次设置](setup.md)操作。
4. 初始化成功后访问 `/api/v1/meta`，记录 `instance_id` 到私有安装记录。核对 `/setup/` 已关闭；根页应显示登录/首页，`/help/` 应能阅读并返回主页，不能把新版首页 404 当成正常。使用原管理员口令和 TOTP 登录，再按[日常操作](daily.md)做一次有权限的精确读取。

成功标志：`prepare` 退出 0，core/gateway 持续运行，meta 返回 200，初始化后 setup 返回 404；设备管理员进一步确认内部 readyz 的 `ready` 和 `business_ready` 均为 true。容器 Running 单独不能证明业务就绪。

## 失败时怎么办

- GUI 不支持 `service_completed_successfully`、挂载或变量展开：停止在部署前，交由设备管理员确认支持的方式；不要去掉依赖条件强行启动。
- `permission denied`：核对目标挂载和服务 UID/GID。不要对旧数据递归 `chmod/chown`，不要使用 777。
- 镜像中找不到 `sh`、`cat` 或 `curl`：这是预期的无 shell 核心镜像。使用 NAS 文件管理器或已有的宿主管理终端；不要把“容器终端”当作必经入口。
- 证书警告：核对域名、证书链、有效期和设备时间。不要忽略警告输入密码；私有 CA 由管理员可信分发。
- 旧实例突然出现设置页：立即停止，不创建管理员。按[升级异常](upgrade.md)核对数据和密钥挂载。

安装后把原项目配置、实际卷名、挂载来源、实例 ID、镜像清单和证书续期方式保存在管理员私有记录中。备份还须单独保存密钥；项目导出不是资料备份。
