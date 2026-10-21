# r10 候选：极空间 GUI 配置

仅用原项目界面导入本地镜像、编辑 Compose、停止与重建，不要求 NAS shell。镜像尚未发布注册表，不能拉取。配套镜像为 __CORE_IMAGE__；实际 image ID、源码提交和 Web 完整摘要见 manifest.json，不能沿用旧云构建的 image ID。先核对 manifest 的 fully_accepted：若为 false，本包只保留已构建候选与独立检查证据，浏览器验收失败，不能当作完整验收通过；详情见 delivery-image-smoke.json 与 Actions 终态。

先在原网页完成并核验完整备份，保存原 Compose/env、旧核心镜像及旧 web。暂停写入，在原项目界面停止 core/gateway。通过现有镜像导入界面导入本批核心镜像归档；若界面只支持 TAR，可先在普通电脑解压 gzip 得到 TAR。用文件管理界面保存旧 web，再将同批 Web 包里的完整 web 目录放到原位置。原 Caddyfile、配置文件、setup、TLS 和备份不覆盖。

用户原配置的最小改动：prepare/core 都改为上述同一新镜像；core 的 start 参数增加 --config-template /etc/lantai/config.template.yaml；原 config.container.yaml 只读 bind 的容器内目标改为 /etc/lantai/config.template.yaml，宿主来源保持原值。保持原项目身份、主目录、env、卷键与实际卷来源、TLS 和32444端口，原 gateway 沿用。然后在原项目界面应用配置并重建，不能只重启旧镜像。

公开模板不含用户真实域名或备份路径，使用 LANTAI_DOMAIN 和 LANTAI_BACKUP_DIR 占位；这些值必须从原私有配置保留，不能用示例替代。用户专用最小差异配置通过私有会话提供。不要用公开模板另建新项目或空卷，也不要把电脑文件位置当 NAS 路径。

r10 start 在服务监听前自动调用与 prepare-config 相同的配置准备逻辑。已有有效持久配置保留；旧只读布局仅在实例/库/schema/原密钥/认证因子可核验时迁入原配置，不创建替代密钥或管理员，不执行数据库格式迁移。失败就停止并保留现场，不删配置、卷或事务文件。

在容器界面核对实际新 image ID；浏览器严格 HTTPS 检查 meta 成功且 instance_id 不变、setup仍404、首页和帮助可用、原管理员登录和原固定版本读取正常。meta 不返回构建版本；Running不代表所有业务ready。不新增公开9090。用合成小文件验证写入后再恢复任务。

本批候选沿用既有打包脚本及其合成安装/原rc5基线升级测试，不能推广为用户当前rc6卷升级或反向回退已验证，也不等于真实DCC编辑验收。失败时先停止新服务并保留现场，恢复旧Compose/env/Web只是回退准备；配置准备及新业务/控制日志可能已经写入原卷，缺少对应反向兼容证据时不要让旧核心强行接管，使用已核验备份的独立恢复方案。

本 artifact 仅选择安装镜像、Web、配置和脱敏证据；不含源码测试、测试现场、会话、口令、TOTP、私钥、数据库、备份包或截图。源码冻结内容及仅打包覆盖层可从本次专用候选提交审阅。本候选不合并main、不创建正式Release、不发布容器仓库、不操作用户电脑或NAS。
