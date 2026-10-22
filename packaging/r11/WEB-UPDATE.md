# r11 Web 导航与人工验收帮助更新

本包只更新 Web。兼容核心为 lantai:v0.1.0-r10-candidate-3ad86ea10830，image ID sha256:38d2114be464e0ba333f2bf4b0a3e88cfbd41bed5b474728e2513df644034945。不更换核心/prepare 镜像、不运行 migrate/init、不改变 schema、项目、卷、端口、TLS 或实际授权。

侧栏先首页、我的项目，再保留五个管理入口及使用帮助。新增 /help/acceptance.html；帮助按创建项目与权限、五入口步骤、实际证据、失败与停止条件组织。文档及目录无需 JavaScript，搜索需 JavaScript。

更新前由设备管理员核对 manifest、SHA256SUMS 和兼容核心，私下保留原完整 Web 目录、原静态 bind 来源及网关配置。把本包 lantai-v0.1.0-r11-web-help-web.tar.gz 解到新的独立目录，得到 web/index.html、assets 和 help；完整替换同一套文件，不能混用两套 assets。

在原项目把现有 Web bind 的宿主来源切到新 web 目录，容器目标仍为 /srv/lantai-web:ro；若来源固定为项目内 web，先保留原目录，再整体替换它。保留 create_host_path:false 和所有原私有值。仅在需要时让原 gateway 的静态挂载重新生效，不重建 core 或数据卷。浏览器重新加载后检查首页/我的项目次序、使用帮助、人工验收与已知固定版本。

只回退 Web：把同一个静态 bind 恢复到保留的旧完整目录，再使 gateway 挂载生效。包内 rollback/ 提供准确原 r10 Web 归档，SHA256 为 0c21322ab7506562dc0dc32e53c63b51aef6fd48184a93127ca9f3fd40707dfd；按同样方式解到独立目录使用，不能覆盖唯一的新旧副本。Web 文件回退不会撤销已提交资料或权限操作，也不是数据库回滚。

出现 TLS、实例、权限、摘要、缺卷或恢复错误时停止；不删除锁、数据卷或版本记录。schema14→15 后禁止同卷直接换旧核心降级。独立旧格式恢复涉及 recover-admin、绑定证据的 restore-complete 和目标配置；NAS 纯 GUI 旧版恢复尚未验证，本次没有新增该能力。

验收人员登录自己的原 HTTPS 站点，从使用帮助→人工验收开始，仅使用批准的合成项目与文件。填写预期/实际/证据/通过失败未测阻塞；不记录口令、动态码、恢复码、会话令牌、私钥或临时下载链接。项目创建回执不自动授予资料角色；有角色仍需当前会话写入范围。备份生成与独立恢复分开验收。

最终测试范围见 manifest 与浏览器报告。未实际执行 NAS 部署或用户电脑操作。未合并 main、发布正式 Release 或推送镜像注册表。

## 冻结制品与后续集成验证

已交付 r11 Web-only 的源提交固定为 `9fa0a60993b35a31f0b66ac29896d6ef36cffe0c`。`r11-web-help.yml` 无论由候选分支推送还是手动复验触发，都检出并核验这个精确提交；制品和证据名称使用冻结源提交，不把触发工作流的后续提交冒充为 r11 Web-only。原有后端文件、模式与变更范围断言保持不变。

main 集成可另含工程脚本或测试适配，应由 `ci.yml` 与 `release-artifacts.yml` 验证当前集成提交。冻结 r11 复验通过不代表这些后续改动通过，也不改变原交付 manifest、核心配对或现场验收范围。上文未合并 main 等说明属于原交付时点，不用于判断后续 Git 合并状态。
