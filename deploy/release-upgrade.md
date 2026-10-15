# 原地升级：rc.5 到包含备份管理的候选版

默认源版本为正式 rc.5 `d27a06fe0af55e205a19e8f95f0764167e217741`。目标候选版的 SHA/image ID/版本与 rc.5→目标的直接升级、备份和回退证据以本次 manifest 为准；候选包生成不等于正式发布或 NAS 验收。仅在原项目操作。

## 核对原实例和保护点

导出原项目有效 Compose 和私有环境配置，私下记录项目名/目录、实际镜像 ID、数据/密钥卷名或 bind 来源、setup/config/TLS/Caddyfile 挂载、域名及原 HTTPS 端口。通过实际挂载识别卷，不能凭项目名猜测。

记录 meta instance_id 和一个原资源版本/摘要；已有实例 setup 应为 404。缺卷、异常或出现设置页时停止排查。暂停写入并优雅停止 gateway/core，按原运维流程做核验过的离线资料备份与独立密钥保护点。保留旧镜像和配置，不删除项目数据、不 down -v、不递归改已有卷权限。

## 应用程序与静态层增量

1. 校验导入新 core 镜像，prepare/core 同时改为同一引用，保留 pull_policy never；实际 image ID/OCI revision 与清单一致。
2. 核验 Web/help 归档并解到版本独立目录；不挂源码根、资料或密钥目录。
3. 保留原 Compose 和 `.env`。rc.5 已有 gateway 静态只读 bind，只更新 LANTAI_WEB_DIR 到核验后的目标版本目录；保留 read_only 和 create_host_path:false。仅在原来缺少该挂载且兼容证据明确覆盖时才新增。GUI 不支持覆盖层时按下节增量修改，不用新装 compose.yaml 或 env.example 覆盖现场。
4. 对比发行 Caddyfile；rc.5 已有 `@web`、file_server 与安全头块，仅合入实际差异。保留原监听域名/端口、bind、证书、API/transfer 上游、日志脱敏及最终 404，不增加 catch-all、9090 或 HTTP 端口。已核验的 Caddy 2.11.4 无需仅为静态层换镜像。
5. 保持原项目名/目录、原 data/secrets 卷身份、setup/config/TLS 挂载、网络、UID/GID、端口和精确 allowed_origins。不切空卷、不 init、不重新建立管理员，不换 TOTP/恢复码，不签发或续期真实身份。
6. 在原项目更新并重建必要容器；只重启旧 core 不换镜像。网关管理端口关闭时按既有停止/重建流程应用配置。

## GUI 单文件需要合入的静态挂载

保持 GUI 的原项目名、项目主目录、环境文件选择以及逻辑卷标识不变（包括自行命名的 data/secrets 键）。原 `services.gateway.volumes` 已有下列静态挂载时只更新环境中的版本目录，不重复添加；已有 Caddyfile/TLS 挂载继续保留：

```yaml
      - type: bind
        source: ${LANTAI_WEB_DIR:?set the verified static web directory}
        target: /srv/lantai-web
        read_only: true
        bind:
          create_host_path: false
```

只在原私有 env 中更新（缺少时增加）`LANTAI_WEB_DIR=实际版本目录/web`，并更新原 LANTAI_IMAGE。路径指向能直接看到 index.html 的目录，不是外层解压目录；不复制 env.example 中其他值。GUI 若不支持该 bind 语法，停止并确认设备支持方式，不能静默改成会创建空目录的挂载。

原 Caddyfile 已有静态块时对比差异；缺少时完整复制发行文件的 `@web path / /assets/* /help /help/*` 与紧随其后的 `handle @web { ... }` 块（包含完整 header 和 file_server），置于最终 `handle { respond 404 }` 之前；不复制外层域名/TLS或其他路由。保存后先核对差异，再在原项目执行应用/重新创建。GUI 按钮名称因设备不同，以“更新配置并重建容器”的实际行为核对。

## 检查后恢复写入

核对 image ID/revision、原卷和全部挂载。meta instance_id 与原值一致、setup 仍 404；通过既有本机方式查 health/ready，不公开 9090。用原管理员、口令及 TOTP 登录，读取原同一资源版本、下载比对摘要，再检查受限账户、首页、帮助返回及预览授权。普通过期会话按既有登录流程处理，不重新初始化身份。

安装静态包后根页应提供登录/首页；不能照旧将 404 一概当成“无业务网页”的正常结果。帮助/静态缺失时排查挂载和清单，不删卷重装。

## 停止与回退条件

设置页重开、instance_id 或卷身份改变时立即停止并保留现场，先恢复原挂载。回退保护点同时包含旧 core、旧 Caddyfile 和旧静态挂载，不能只换镜像留下不匹配静态层。

只有发行证据实际包含目标→同一 rc.5 镜像的反向验证，且 schema/资料/配置没有迁移、没有其他恢复或格式修改时，才按该版本对的说明停写停机、恢复原程序/静态配置并完整核对。没有对应证据就记未验证；其他版本对的回退不继承。自动反向验证覆盖升级后的登录、读取、预览、下载及在线备份，没有覆盖任意新增业务写入后的回退；回退不撤销已接受写入，不推广任意旧版；恢复备份会重建身份，与保卷升级不同。

## 可选：在获准的升级窗口启用在线备份

本节仅适用于明确包含 `backup-online`、且通过对应环境验收的目标版本；不追认其他历史版本对的证据。先保持原数据/密钥卷身份，核验目标镜像实际命令，再按[在线备份](../docs/online-backup.md)准备私有外部备份目录及 `.lantai-backup-root` 标记，在原 core 一次性加入 `compose.backup.yaml` 的 environment 和 bind。GUI 单文件的增量片段也在该说明中。只在原 core 加两项 environment 和一个 bind，并在原私有 env 增加 LANTAI_BACKUP_DIR、LANTAI_BACKUP_ROOT_ID；不替换原 volumes 声明或自定义逻辑卷键，不切换到新空卷。core/gateway 的既有 /tmp tmpfs 若缺少 mode=1777，仅补该参数，保留其他限制。

挂载作为这次经批准升级的一部分应用；日后由网页“备份管理”立即备份、查看历史、取消/续传或设置每日计划。已有本机运维终端仍可用 `backup-online` 操作同一协调器。不需要逐次编辑 Compose、另起备份维护服务或手工停启 core。备份期间业务读写暂停；失败/取消等待实际工作退出后恢复。旧 `backup` 继续要求停机和实例独占锁，不能在活动 core 旁使用。

代码复核和合成容器测试不代表原 rc.5 卷升级/回滚、NAS 文件系统或真实数据恢复已经验收；发布和现场升级由主流程单独协调。

仍运行 rc.4 的实例须先使用对应 rc.4→rc.5 的已核验流程升级，或取得覆盖其源版本的独立升级证据；本包的 rc.5 基线不自动覆盖 rc.4 直接升级。

新管理控制日志包含旧 rc.5 解析器不认识的字段。回退证据只覆盖恢复原先未启用备份控制的配置和普通服务；不能让旧 `backup-online` 控制器接管新日志。若原 core 已启用旧在线备份，先另做对应配置和控制日志兼容核验；不删除新日志来伪造兼容，也不推广本包的回退结论。
