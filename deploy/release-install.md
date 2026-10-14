# 新装：核心、网页和帮助统一交付

只用于没有既有实例的新项目。已有用户请读[原地升级](release-upgrade.md)，不能用新装配置替换原项目。

1. 下载同一发行的完整制品，校验 SHA256SUMS、manifest 版本/source_commit、平台、成功 main CI 和发行验收。导入核心/Caddy 镜像后核对实际 image ID，不能只凭 tag。
2. 将 Web 与 deployment 两个归档解到独立项目目录，得到 `web/` 和 `deploy/`。静态目录不放资料、密钥、私有配置或证书；NAS 无需 Node。
3. 复制 env.example 为私有环境文件，填写实际 TLS 域名/端口、config 和独立 setup 目录，LANTAI_WEB_DIR 使用 web 的绝对路径。TLS 私钥由设备管理员另行保留，allowed_origins 精确匹配同一 HTTPS 地址。
4. 使用 compose.yaml 与 compose.web.yaml 创建新项目；GUI 只支持单文件时合入 gateway 静态 bind。若经审查的目标镜像包含 `backup-online`，还可按[在线备份](../docs/online-backup.md)在同一次部署中加入 compose.backup.yaml，或一次性合入原 core 的环境和备份 bind。先准备私有备份根及标记；缺失挂载应拒绝启动，不将空目录当成成功。已发布 rc.4 没有该命令。
5. 在 HTTPS `/setup/` 完成受保护登记，手动绑定 TOTP、保存恢复码并确认动态码。meta 正常且 setup 永久 404 后，根页使用原口令/TOTP 登录，`/help/` 无需登录阅读。
6. 核对项目、资源、显式预览及权限；新空馆显示空态正常。API/transfer 在内部网络，9090 不公开；不加公共 HTTP 端口或关闭证书校验。

NAS GUI、文件系统、权限及实际客户端信任需设备管理员验收。Running、首页可打开或制品校验通过不代替资料/密钥备份和目标环境验证。
