# 升级并保留原实例

目标：更换程序镜像，同时保留原资料和身份。前提：设备管理员已确认源版本、目标版本及兼容说明；当前实例健康，有与源版本匹配、经过核验的备份和独立主密钥副本。以下在 **NAS 原来的项目**操作，不建立替代的新实例。

## 1. 先记录原来的真实状态

在 NAS 项目界面导出当前 Compose 和有效环境变量，私下记录：

- 原项目名称和目录、core/gateway 容器 ID、镜像 image ID 与 OCI revision。
- core 的 `/var/lib/lantai`、`/var/lib/lantai-secrets` 的实际卷名或宿主路径。
- config、setup、Caddyfile、TLS 的实际挂载来源，以及原域名和 HTTPS 端口。
- `/api/v1/meta` 的 `instance_id`；一个可读资源的精确版本 ID 和文件 SHA-256。

用实际挂载判断，不能根据项目名称猜测卷名。镜像 tag 可以被重复使用，不能单独证明版本。已有实例此时 `/setup/` 应为 404。若已有异常、缺卷或出现设置页，停在这里先排查。

## 2. 停写、停服务、做保护点

1. 暂停上传、任务和客户端自动重试。
2. 在原项目停止 gateway，然后优雅停止 core。选择“停止”，不选“删除项目及数据”。
3. 旧 rc.4 按[离线备份](backup.md#旧-rc4-的离线流程)对原实际挂载执行 backup 和 backup-verify，并另存原主密钥；若源版本已经包含并配置在线备份，可先完成同 core 的 backup-online 和独立校验，再进入本次停机升级窗口。备份与核验都成功后才继续。
4. 保留旧镜像和原项目配置。导出 Compose 或容器文件系统都不能代替资料与密钥备份。

GUI 若不能确认优雅退出，应使用已有管理终端的 `docker stop --timeout -1 CONTAINER_ID`，等待正常退出；异常退出或一直不退出时保留现场，不直接升级。

## 3. 从当前 rc.4 更新核心、静态包与网关

本次候选包的默认基线是 rc.4 `d005b2d`。先核对 manifest 的 baseline_commit、direct_upgrade_passed、backup_smoke_passed 和 rollback_passed，不沿用其他版本对的结果：

1. 导入同一目标发行的已核验核心镜像，原 prepare/core 同时改为同一引用，保留 pull_policy never。
2. 解压并核对 `*-web.tar.gz` 到版本独立目录，得到 index.html、assets 和 help；不挂源码根、资料或密钥卷。
3. 保留原项目 Compose 和 `.env`。rc.4 已有 `/srv/lantai-web` 只读 bind，只将 LANTAI_WEB_DIR 更新到新静态目录；保留 read_only 和 create_host_path:false。GUI 中保留原项目名、全部逻辑卷键和实际卷来源，不用新装模板替换原项目或切空卷。
4. rc.4 已有静态路由；对比后仅将发行 Caddyfile 的实际差异增量合入原文件；保留原域名/端口、bind、TLS、API/transfer 上游、日志脱敏与最终404。不新增公共端口、不开放9090或catch-all。
5. 保持原项目名/目录、实际数据/密钥卷、config/setup/TLS挂载、网络与原HTTPS端口。原管理员、口令、TOTP和恢复码不改；不 init 或签发/续期真实身份。
6. 如启用在线备份，按[备份](backup.md)一次性加入外部私有目录、标记及原 core 的两项环境和一个 bind。既有 /tmp tmpfs 补 mode=1777，保留其他限制。之后每次直接在同一 core 执行 backup-online，无需再改配置或停启 core。
7. 在原项目按新镜像重建 core，并重建必要 gateway 使新配置生效；只重启旧 core 不更换镜像。原已核验Caddy2.11.4可沿用。

完整交付说明见[原地升级模板](../../deploy/release-upgrade.md)。prepare 对已有内容只校验，不证明选中的是原实例。不要把未实施的卷绑定设计混入升级。

## 4. 检查后才恢复写入

1. 核对新 core 的实际 image ID 和 OCI revision，不能只看标签或 `(devel)` 版本文字。
2. 比对原数据/密钥卷身份及全部挂载来源、域名和端口。
3. meta 返回 200，`instance_id` 与升级前一致；setup 仍为 404。
4. 管理员通过已有的本机观测途径核对 `/healthz` 为正常，`/readyz` 中 `ready=true`、`business_ready=true`、`state=ready`、`reason_codes=[]`。不要为此公开 9090。
5. 使用自己的有效会话读取并下载升级前同一个版本，核对 SHA-256。普通过期会话可按原流程重新登录，不重新初始化管理员。

缺少内部探测工具或有效会话时，把相应检查记为“未验证”。脚本退出 0、容器 Running、首页 404 都不能替代以上验证。

## 失败与回退

出现 setup 页、实例 ID 改变、卷身份不同：立即停 core/gateway，保留现场，不建立新管理员；恢复原挂载后再检查。别把误建的空卷继续当成原实例。

rc.4 `d005b2d` → 目标候选版直接升级和条件反向回退，只以本次实际候选包的 delivery_smoke 证据为准；不能用历史版本对的测试代替。只有对应版本对的反向验证、无格式迁移及未做其他恢复/格式修改时，才停写停机恢复旧 core、旧 Caddyfile 和旧静态配置并完整核对。自动反向验证覆盖升级后的登录、读取、预览、下载和在线备份，没有覆盖任意新增业务写入后的回退。回退不撤销已接受写入，不推广任意旧版，不证明现场 NAS 已升级。

若发生迁移、较新格式拒绝、损坏或密钥不配对，先停止，按兼容核验备份恢复流程处理。整馆恢复会重建认证身份，不等于保留原登录的普通镜像回退。
