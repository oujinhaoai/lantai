# deploy · 部署模板

归属：[T08 运维与部署](../docs/tasks/T08-operations-deploy.md)。

存放可公开的容器、HTTPS 网关、系统服务管理与配置模板。模板仅使用示例域名和变量占位，真实配置在仓库之外。

基线为单网关入口、同一核心进程内的 API/传输/本机运维监听；运维面不经网关发布。SQLite 使用服务端本机文件系统。M7 才增加推送长连接面。

入口为[部署与本机观测](../docs/deployment.md)。`Caddyfile` 用于手动证书的单 HTTPS 入口；`config.native.yaml`、`config.container.yaml`、`compose.yaml`、`Dockerfile` 与 `lantai.service` 是需要在目标环境审查的通用模板。

`gateway_test.go` 在显式指定已核验的 Caddy 2.11.4 二进制后，用随机 loopback 端口、临时证书和合成后端验证实际模板。容器引擎、systemd、目标 NAS 与其他平台必须分别验收，不能由本地网关测试推导。
