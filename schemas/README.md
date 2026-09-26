# schemas · 数据与执行协议

归属：[T00 工程与契约](../docs/tasks/T00-foundation.md)，由相关领域任务共同维护。

计划维护资产清单、著录、来源/许可/检查证据、事件、插件、Agent 执行与结果的 JSON Schema。每种对象有稳定 schema 标识、版本、正反例和兼容性规则。

HTTP 路径、认证方式和状态码放在 `api/`；共同的数据定义通过引用或受控生成复用。外部后端的原生 schema 不作为兰台的业务契约。当前尚未提交可用 schema。

[T09](../docs/tasks/T09-extension-platform.md) 负责 `extension.yaml` / `lantai.extension/v1` 统一扩展清单，processor 字段合入其中；T00 维护公共版本规则。M1 包含扩展点 ID 及产物/证据的插件 ID、版本、包摘要，CLI/Web 字段仅预留，声明必需能力不受支持时明确拒绝。配置、常驻控制或跨插件依赖协议随实际阶段和需求补充，不以 schema 预留推导已实现能力。插件实现规格以[扩展设计](../docs/extensions.md)为唯一权威。
