# 文档导航 · Documentation

状态：开发起步阶段，已有 Go 工程、公共契约、真实存取与身份服务、事件查询、共同备份恢复、运维模板和内置扩展登记。除项目 README 外，首轮工程文档以中文维护；协议字段与代码标识采用英文。

The project overview is available in [English](../README.en.md). Engineering documents are initially maintained in Chinese; protocol fields and code identifiers use English.

| 文档 | 用途 |
|---|---|
| [中文 README](../README.md) / [English README](../README.en.md) | 项目定位、架构方向、目录和路线 |
| [架构与一级目录](architecture.md) | 目录边界、服务模块、数据归属、协作规则 |
| [开发任务基线](tasks/README.md) | T00–T09 的公开范围、职责、阶段、依赖与验收要求 |
| [公共契约](contracts/README.md) | 标识、摘要与规范化、幂等回执、错误模型、事件、操作阶段、跨模块接口、所有权与版本规则；[错误码表](contracts/error-codes.md)、[所有权](contracts/ownership.md)、[执行与扩展宿主的公共约定](contracts/execution.md)、[实例生命周期与维护屏障](contracts/instance.md)、[身份与授权](contracts/identity.md)与[动作和策略登记](contracts/identity-actions.md) |
| [共同备份与恢复](contracts/backup-restore.md) | 四库共同点、永久 pin、空目录恢复、凭据轮换与对账 |
| [单网关部署与运维](deployment.md) | Caddy / 容器 / systemd 模板、内部监听和本机指标 |
| [开发与验证](development.md) | 真实可运行的构建、测试、生成、检查命令与平台验证状态 |
| [依赖与许可证](dependencies.md) | 锁定的依赖与工具版本、许可证核对与选型依据 |
| [架构决策记录](adr/README.md) | 本轮基线、被替代的建议与支持边界 |
| [三端扩展设计](extensions.md) | 插件实现规格的唯一权威：清单、扩展点、分期宿主、包治理与安全边界 |
| [插件系统参考评估](references/plugin-systems.md) | DeepSeek Harness 固定源码及六个外部项目的官方机制与取舍 |

仓库里的设计是重新编写的公开版本，示例仅使用合成数据。内部研究、真实环境与验收原始数据不复制到此处。任务进度唯一维护在维护者 Obsidian 的 Lantai「开发与测试」，负责人、阻塞项与执行证据索引也记录于此，仓库任务卡不重复维护当前状态。实现阶段的公开 ADR、契约、合成测试与脱敏验收证据由 T00 建立索引，知识库执行记录引用这些证据；行为变更同时更新契约、相关任务基线及中英文 README。
