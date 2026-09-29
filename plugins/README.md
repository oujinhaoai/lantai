# plugins · 官方扩展包

归属：[T09 扩展平台](../docs/tasks/T09-extension-platform.md)负责统一包与宿主契约；[T06 执行与节点](../docs/tasks/T06-execution-nodes.md)负责处理器/Agent 的业务调用。

M1 只登记内置服务端/节点 processor、validator；M2 用文件协议 spawn/run/exit 实现一个可重复验证的一次性结构检查插件，不要求常驻控制协议。M3 按能力扩充媒体与 3D 处理器。源码、依赖说明、插件清单和测试随各插件维护，语言按处理工具选择。

插件只接收获授权输入，输出结构化结果、摘要与证据，不能直接访问核心五库或规范素材目录。协议 schema 在 `schemas/`，宿主实现属于 `internal/`。大文件及第三方模型权重不随目录占位加入仓库。

包清单只有 `extension.yaml`，schema 为 `lantai.extension/v1`，processor 字段并入其中，不另设 `plugin.yaml` 或 `module.yaml`。CLI/Web 字段在 M1 只预留；M2 接显式 CLI，M3 接官方/声明式 UI，第三方网页及跨插件服务依赖按需立项。网页宿主和首方应用组件在 web，不把整个网页应用放入插件目录。插件实现规则以[扩展设计](../docs/extensions.md)为唯一权威，本目录不自动安装或运行插件。

当前 `corecheck/` 是随核心编译的纯 manifest 结构校验器。`packages.go` 显式嵌入清单、配置 schema、许可证据和源制品，由 `internal/extensions` 在维护屏障内统一登记；它不扫描其他目录。修改包文件须更新清单中的文件 SHA256/大小并升级版本。包内容摘要与当前可执行制品摘要分别绑定，产物/证据带 `builtin_release` 和两个摘要；算法及接口见[内置登记契约](../docs/contracts/extensions.md)。合法检查不合格不会变成人审批准，M1 不执行外部 processor。

M2 的受信任 corecheck 已由 `internal/extensions/oneshot.go` 通过当前核心二进制的私有入口运行；独立 Job 和证据接受归 T06。此路径不开放外部包导入或通用沙箱，见[手动执行与检查作业](../docs/contracts/manual-execution.md)。
