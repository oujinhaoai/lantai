# 文档导航 · Documentation

状态：第一版开发准备材料，待确认后进入实现。除项目 README 外，首轮工程文档以中文维护；协议字段与代码标识采用英文。

The project overview is available in [English](../README.en.md). Engineering documents are initially maintained in Chinese; protocol fields and code identifiers use English.

| 文档 | 用途 |
|---|---|
| [中文 README](../README.md) / [English README](../README.en.md) | 项目定位、架构方向、目录和路线 |
| [架构与一级目录](architecture.md) | 目录边界、服务模块、数据归属、协作规则 |
| [开发任务](tasks/README.md) | T00–T09 任务卡、依赖、首批开发与确认范围 |
| [三端扩展设计 v2](extensions.md) | 服务端、客户端、网页扩展点、包治理、宿主、权限和生命周期 |
| [插件系统参考评估](references/plugin-systems.md) | DeepSeek Harness 固定源码及六个外部项目的官方机制与取舍 |

仓库里的设计是重新编写的公开版本，示例仅使用合成数据。内部研究、真实环境与验收原始数据不复制到此处。实现阶段的 ADR、契约和验收记录由 T00 建立索引；行为变更同时更新契约、相关任务及中英文 README。
