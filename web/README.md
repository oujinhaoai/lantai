# web · 独立网页工程

首个实现阶段：M3。计划使用 TypeScript/React，工程与依赖在启动该阶段时建立。

首版范围是登录、待审证据与决定、任务状态和回收站，复用 M2 已成立的 API 与敏感动作授权。网页独立构建/部署，经网关与 API 同源；不访问核心数据库或数据目录。

转码预览、标注与版本对比留 M5。本目录目前只有职责说明，详见[后续工作](../docs/tasks/README.md#later-work)。

[T09](../docs/tasks/T09-extension-platform.md) 在这里实现 UI registry、设置 schema 表单和 broker，预留预览、侧栏、动作、任务结果、页面等插槽。首方 React 组件随应用发布；第三方视图经独立 origin 的 sandbox iframe 和有界 broker 访问能力，验证隔离后才启用。插件 origin 不承载主站会话，各不互信包不能共用 origin；单网关端口可按主机名分流。登录、人审、插件启停和权限管理由宿主提供，详见[三端扩展设计](../docs/extensions.md)。
