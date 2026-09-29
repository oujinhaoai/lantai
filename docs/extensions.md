# 服务端、客户端与网页扩展设计 v2.1

状态：**M1 清单、扩展点与官方内置静态登记，以及 M2 一次性宿主、包治理、熔断与显式 CLI 已实现**（接口见[扩展包治理契约](contracts/extension-governance.md)）；常驻实例、跨插件依赖、节点侧宿主与网页宿主按阶段或需求建设。本篇细化插件机制，不自动授权安装外部插件或启用自动化。现行目录、Go 单核心进程、五库和核心裁决规则保持不变。参考证据见[插件系统调研](references/plugin-systems.md)，实施拆分见 [T09 扩展平台](tasks/T09-extension-platform.md)。

本篇是插件实现规格的唯一权威入口，随代码评审和版本演进；`schemas/` 在实现时承载机器可校验契约，本篇引用它而不复制第二套字段定义。知识库只保留摘要、私有背景、设计决策与执行追踪。2026-09-26 的 v2.1 收敛 M1 范围，明确一次性/常驻协议、统一包文件与熔断默认值；通用依赖框架和第三方网页代码改为按需。

## 1. 评估结论

现有文件处理协议作为起点。M1 只实现统一清单 schema、扩展点 ID 表、官方内置组件静态登记，以及产物/证据中的扩展 ID、版本和包摘要。先固定服务端/节点的处理器与校验器字段，CLI/网页仅预留 target 与贡献命名空间；遇到未支持 target、扩展点或非空插件依赖声明，启用时明确拒绝。其余机制按第 13 节实施，不作为数据底座的前置。

采用**统一扩展包与注册规则，分端宿主执行**：

| 层次 | 做什么 | 实现方向 |
|---|---|---|
| 共同控制层 | 包身份、摘要、版本、贡献声明、权限、启用范围与兼容性；依赖及高级配置按需 | 核心 `extensions` 模块与版本化 schema；客户端仅缓存已授权视图 |
| 服务端/节点宿主 | 处理器、校验器、执行后端、连接器、外部能力服务 | 进程外协议；正确性关键驱动仍随官方核心编译发布 |
| 客户端宿主 | 显式安装的子命令、导入导出、DCC 适配和受控 MCP 工具贡献 | 本机注册表、受控进程和薄 SDK，不扫描工作目录后自动执行 |
| 网页宿主 | 预览、侧栏、动作、设置、页面、任务结果视图 | M3 首方组件与声明式 UI；第三方代码宿主另行按需立项 |

核心只提供明确的接口，不引入一个“任意插件都能替换任何模块”的总线。权限最终判断、稳定 ID、提交、台账审定/发布/清除和 Task 租约状态机不能被运行时插件覆盖。业务扩展通过配置、异步任务和候选证据参与这些流程。

## 2. 借鉴与取舍

- DeepSeek Harness / Cordis：借鉴服务定义、提供者与消费者分离，以及所有注册可撤销的生命周期；不照搬“所有核心逻辑都可以被运行时替换”。
- VS Code：借鉴静态 contribution manifest、能力/宿主声明、按需激活与客户端命令注册；extension host 不自动等于安全沙箱。
- HashiCorp go-plugin：借鉴进程外握手、协议版本和宿主回收；首版保持语言中立，不以其具体 RPC 栈作为硬依赖。
- Caddy / Backstage：借鉴命名空间、初始化/验证/清理，以及显式依赖注入与可扩展接口；官方编译模块和外部不可信代码分开治理。
- JupyterLab / Grafana：借鉴可组合视图、设置 schema、面板/数据源和前后端协同；模块联邦或远程 React 组件本身不能构成安全隔离。

这些是兰台对机制的选择，并非宣布兼容上述项目的插件格式。来源、版本范围和具体限制集中记录在[调研文档](references/plugin-systems.md)。

## 3. 服务端需要预留的扩展点

M1 固定扩展点 ID 与领域所有者，并为处理器/校验器定义输入输出及权限契约；扩展登记只支持服务端/节点的 `artifact.processor` 与 `asset.validator` 内置实现。下表其他扩展点是后续目录，不要求 M1 为每项建立完整协议或空服务。未支持的扩展点明确返回 `EXTENSION_POINT_UNSUPPORTED`，不悄悄忽略配置。

| 扩展点 | 输入 → 输出 | 谁验收/裁决 | 实现阶段 |
|---|---|---|---|
| `asset.type` | 类型/schema/文件角色 → 校验后的类型配置 | catalog，配置固定版本 | M1 核心内建类型；外部配置扩展按需 |
| `metadata.extractor` | 固定 manifest → 结构化元数据与证据 | provenance；更新须走条件命令 | M1 核心内建抽取；M3 扩展登记 |
| `asset.validator` | 固定版本+Profile → CheckResult | ledger/Profile，插件只提供结果 | M2 |
| `artifact.processor` | 获授权输入 → 派生文件/记录 | jobs 验收，再走普通入藏 | M2 最小，M3/M5 扩充 |
| `ingest.importer` | 选定外部输入 → 候选 manifest/来源映射 | catalog/ledger，支持 dry-run | M3/M4 |
| `export.connector` | 固定版本+用途 → 外部传输回执 | 核心先检查许可和导出权限 | 按需求，默认关闭 |
| `search.provider` | 获授权查询/候选 → 排名与扩展字段 | query 在输入和输出均检查权限 | M6；M1 仅 ID 预留 |
| `workflow.template` | 声明式步骤和依赖 → 固定 FlowDefinition | workflow 解析与版本锁定 | M2 固定，M3 扩展 |
| `agent.backend` | TaskRun/输入锁/预算 → 状态、检查点与候选 | agent_execution + tasks + ledger | M2 手动，M3 后端 |
| `notification.channel` | 已授权事件投影 → 发送回执 | events 持久出站、幂等和脱敏 | M7 |
| `event.consumer` | 按声明过滤的事件 → ack/后续命令 | 各命令所属模块再次授权 | M3+，先内部消费 |
| `service.api` | 已登记路由/schema → 能力服务结果 | 核心鉴权、配额、请求/响应校验 | 具体能力需要时 |
| `storage.driver` / `file.install` / `db.driver` | 官方接口 → 持久化操作 | 核心及平台测试 | M1 核心静态组装，后续编译扩展 |

搜索扩展不得接收无权对象后靠最后过滤补救。连接器不能借通知通路外传全部资产。自定义路由仅在 `/api/v1/ext/<plugin-id>/` 下注册明确方法和 schema，不覆盖核心路由、不提供任意 URL 代理；目标服务地址由管理员部署配置选择。

不在提交事务里调用第三方同步钩子。需要阻止发布的外部检查先生成证据，由 AcceptanceProfile 在审定/发布时检查。解析超时、schema 错误或能力缺失应产生明确失败或 unknown，不能当成 pass。

## 4. 客户端扩展

| 能力 | 设计 |
|---|---|
| 子命令 | 推荐 `lantai ext run <plugin-id> <command>`；简写别名显式注册，不遮蔽内置命令 |
| 导入/导出与 DCC | 用 SDK 解析用户明确选择的输入或调用标准 API；先生成候选/dry-run，再由核心接受 |
| 输出格式/批量操作 | 命令声明输入、输出 schema、是否修改数据、幂等及恢复方式；不得仅返回无法判断成功的文字 |
| MCP 工具贡献 | 从审定过的命令声明选择性投影，工具名带插件命名空间；限制结果大小与资源链接，写入经原有授权/回执 |
| 本地设置 | 声明 schema、作用域与默认值；工作区配置不能覆盖可执行路径、凭据来源或授予新权限 |

M2 的本机命令贡献使用扩展点 `cli.command` v1（所有者 cli，target 仅 `cli`），输入/输出为 [`lantai.cli-command-input/v1`](../schemas/extensions/v1/cli-command-input.schema.json) 与 [`lantai.cli-command-result/v1`](../schemas/extensions/v1/cli-command-result.schema.json)，同样走一次性文件协议：命令只读取显式选择的输入副本、只向调用者给出的空目录交付核验后的文件，不修改兰台权威状态，需要入库时另行提交。M2 只提供完整形式 `lantai ext run <plugin-id> <command>`，不注册简写别名。

禁止“找不到命令就执行 PATH 中任意同名程序”。显式安装时记录完整解析路径、平台和摘要，调用前核验；工作目录中的同名文件不能劫持命令。包来源、argv 模板和入口都来自已登记元数据，不将任务说明拼接为 shell 命令。

启动环境使用白名单，默认不继承宿主密钥和完整环境变量。一次运行得到限定的工作目录与能力通道；访问核心优先经 broker，确需直接连接时由服务端签发短期、限项目/用途/插件的凭据，不传长期用户令牌。能运行本地代码仍意味着本机信任，不能把 CLI 父子进程关系称作沙箱。

MCP 不开放人审、凭据管理、提前清除、插件启用或任意 shell。插件不得仅凭标注 `readOnly` 获得权限；行为注解由已验证命令属性生成。读取任意外部文件和联网等能力也需单独声明并受宿主策略限制。

## 5. 网页扩展与插槽

M3 交付官方可信组件与声明式界面。第三方脚本/iframe 是按需能力，需独立任务、部署条件与隔离验收，不属于 M3 默认承诺，也不阻塞首方网页。以下第三方约束仅在启动该能力时实现。

### 5.1 第一批需要定义的插槽

| 插槽 | 适用内容 | 约束 |
|---|---|---|
| `ui.asset.preview` | 图片/音频/视频/3D/自定义格式查看器 | 按角色/MIME/能力选择，固定版本，保留下载或基础详情后备 |
| `ui.asset.inspector` | 元数据、来源、检查结果侧栏 | 只取得当前有权字段；不接收整馆对象缓存 |
| `ui.asset.action` | 声明式资源动作 | 按权限和选择上下文显示，执行时核心再次判定 |
| `ui.task.result` | 作业/Agent 结果与证据呈现 | 内容是数据，不执行模型输出的 HTML/脚本 |
| `ui.navigation` / `ui.page` | 能力页面、导航项 | 独立命名空间，不能覆盖登录/审定/权限管理 |
| `ui.settings` | 插件配置表单 | 宿主按 schema 渲染；密钥写入专用入口，不回显给插件 |
| `ui.dashboard` | 项目/任务卡片与统计 | 限定查询和预算，失败隔离到卡片 |
| `ui.annotation` | 时间码、帧、3D 锚点编辑器 | M5；数据经评论 API 保存，不能自建第二份审定事实 |

核心拥有登录、授权说明、人审确认、插件启停与审计页面。插件可贡献导航到这些页面的动作，但确认界面与 HumanGrant 的产生必须由宿主执行，不能用插件自己的对话框替代。

### 5.2 运行信任等级

- **声明式 UI**：表单、动作、列、菜单和静态说明由宿主渲染；首选方式，不执行外来脚本。条件表达式为受限 AST，不使用 JavaScript eval。
- **首方可信组件**：随网页发布的 React 模块，使用显式注册表和宿主 API；有相同来源权限，视同核心网页代码评审。ErrorBoundary 只限制渲染故障，不限制权限或死循环。
- **第三方 UI**：经审定/授权后，在独立、无宿主凭据的扩展 origin 中运行 sandbox iframe，经 broker 获得有限数据。未完成隔离验证时只允许声明式扩展，不回退为同源动态 import。

第三方 iframe 不获得 host token、Cookie、HumanGrant、整个应用 store 或主站 DOM。浏览器 Worker 可承载计算，但不能作为同源代码的凭据隔离方案。第三方 bundle 由固定摘要交付，不从运行时任意 CDN 装代码。

隔离部署可复用同一网关端口，用专用 HTTPS origin 路由静态插件包。不互信的包及不同权限实例不能共用 origin；origin 绑定到包摘要/实例策略，停用或升级按策略清除存储、缓存和 Service Worker，不向新实例复用旧能力。主站 Cookie 使用 host-only、不设置共享 Domain，插件 origin 不承载业务会话。

origin 不同不保证 site 不同。第三方代码部署使用与主站不同的可注册域（eTLD+1）；内网特殊后缀不能仅凭域名拼写认定隔离，需在目标浏览器核验 schemeful site、`Sec-Fetch-Site` 与 Cookie 行为，无法证明跨站时不开放第三方代码。`ext.lantai.lan` 与 `lantai.lan` 仅更换子域，不作为跨站隔离证据。每包独立来源需要 DNS/TLS 和生命周期管理，通配解析/证书只是部署选项，不是 M1/M2/M3 的必备设施；少量来源也可显式配置。

首版 broker 固定使用已登记独立 HTTPS origin，iframe 仅开放 `allow-scripts allow-same-origin`，其余 sandbox 能力默认关闭；这里的来源必须与宿主和其他不互信插件分离。拒绝 `null` 或通配来源，不把 opaque-origin iframe 混入此模式。CSP、frame-ancestors 与 Permissions-Policy 默认拒绝、逐能力放行；三种目标浏览器均须实测。

host-only Cookie 不能单独防止插件诱导浏览器携带主站登录态发请求。业务 API 不允许扩展 origin 的带凭据 CORS；Cookie 写请求验证准确 Origin 与 CSRF，GET 无副作用；插件动作只经 broker。来源隔离、服务器请求校验与 broker 授权共同成立后才开放第三方 UI。

broker 只接受登记动作，逐条校验 schema、请求 ID、插件/激活代次、对象范围和限额。建立通道时核对准确 origin、窗口 source 与握手 nonce；不用 `*` 投递敏感数据。页面导航、注销、切项目、撤权和插件停用都关闭旧通道、取消订阅并丢弃迟到响应。人审动作需要宿主真实用户操作、服务端冻结目标及独立验证，子页面消息不能直接换取人审授权。

SameSite 仅作纵深防护，不能替代上述校验，也不能从跨站推导浏览器进程隔离。浏览器机制依据：[Chrome 对 origin/site 的说明](https://web.dev/articles/same-site-same-origin)、[iframe sandbox](https://html.spec.whatwg.org/multipage/iframe-embed-object.html#attr-iframe-sandbox)、[Web messaging 来源与消息校验](https://html.spec.whatwg.org/multipage/web-messaging.html#security)。具体部署门禁、broker、权限与代次规则是兰台的设计。

## 6. 统一扩展包

唯一包清单文件为根目录的 **`extension.yaml`**，契约标识为 **`contract: lantai.extension/v1`**。原 `plugin.yaml` 的处理器字段并入对应 target 的 `processor`；服务模块也使用此文件，不另设 `module.yaml`。旧格式仅可由未来显式迁移工具转换；新宿主拒绝双清单或含义冲突，不按读取顺序决定优先级。

M1 的机器字段见 [`lantai.extension/v1`](../schemas/extensions/v1/extension.schema.json)，内置组件见 [`plugins/corecheck`](../plugins/corecheck/)，实现与校验示例见[扩展契约](contracts/extensions.md)。`package_digest` 固定清单及组件源制品的文件清单（路径、大小、SHA-256）的 JCS 摘要；`core_release_digest` 固定正在运行的可执行制品。相同 ID/版本不能更换包摘要；同一不可变包可绑定多个核心发布摘要，重新构建不改写旧包身份或历史证据。

一个发布包可包含多个 target，但各 target 独立审查权限、激活与故障处理；服务器启用不等于在用户电脑或网页自动执行。以下为 **M2 一次性校验器的结构草案**，省略平台制品、各文件摘要等完整字段，不是可安装包；M1 定 schema 和官方内置登记，不启动示例中的外部入口：

```yaml
contract: lantai.extension/v1
id: org.example.asset-tools
version: 0.1.0
compatibility:
  host_api: ">=1.0.0 <2.0.0"   # 实际宿主版本在 T00 固定
  required_points:
    - { id: asset.validator, version: 1 }
contributes:
  - id: org.example.asset-tools.check
    point: asset.validator
    target: node
    input_schema: lantai.check-input/v1
    output_schema: lantai.check-result/v1
targets:
  node:
    runtime: exec
    lifecycle: oneshot
    entry: bin/checker
    protocol: lantai.processor/v1
    processor:
      accepts: { extensions: [fbx] }
      requires: [ufbx-inspect]       # 宿主工具能力，不是另一个插件
      timeout: 120s
      concurrency: 2
      produces: { records: [fbx.inspect/v1] }
permissions:
  api: []                          # worker 验收并提交，处理器无核心令牌
  network: []
  filesystem: ["granted-input:read", "run-output:write"]
  secret_refs: []
dependencies: []                   # 前期只接受空的跨插件依赖
config_schema: schemas/config.json
```

M1/M2 不用上述 CLI/Web 预留字段激活尚未交付的宿主。M2 支持 CLI 后，按同一包格式声明其 target/贡献/协议，不另加一份清单。每个贡献有稳定命名空间 ID，重复 ID 拒绝；所有实际执行入口按平台显式登记文件路径和摘要。

完整清单必须声明兼容范围、平台制品与文件摘要、权限和资源上限、配置 schema、许可证据；有自有状态时再声明状态 schema 与恢复方式。包整体摘要由导入/发布过程对实际制品计算，存于登记和调用/证据记录，不在被散列的文件内递归填写自身摘要。同版本不同包摘要禁止覆盖。内置组件也记录扩展 ID、版本、对应发布制品摘要及 `builtin_release` 来源，运行产物与检查证据能追溯这些值。

`processor.requires` 描述本机工具/运行能力，绑定管理员登记的路径、版本与摘要，不自动从 PATH/CWD 搜索执行。静态检查能确认的能力不运行代码；必须执行探测的情况按第 11 节获授权后进行。探测结果不能授予新权限。

分发格式统一，调用协议按形态区分：声明式配置、`processor` 一次性文件协议、Agent adapter 的 TaskRun 业务协议、常驻 service API、CLI 命令协议、web broker。包清单是声明，`job.json`/`result.json` 是单次调用数据，不是第二份包清单。

## 7. 服务定义、依赖与作用域

### 7.1 M1/M2 的最小边界

M1 用 Go 显式组装与静态表登记内置组件，检查 ID、版本/摘要、target 和扩展点是否支持；没有通用 DI 容器、跨插件依赖图或可热卸载的服务容器。内置组件随核心进程启动/退出；正常的资源关闭仍由所属模块负责，不另建通用 dispose 框架。M2 新增一次性宿主及 CLI 注册后，仅实现这些真实资源的撤回和清理。

每次调用显式携带主体、项目、用途、输入版本与 deadline，不使用可变全局“当前用户”。前期插件间 `dependencies` 只接受空值；需要跨插件服务时先设计实际消费者/提供者用例，再通过独立任务启用相应契约，不因 schema 预留字段就接受配置。

配置采用该组件的明确 schema 和冻结后的有效配置。M1 不实现多层合并；M2 管理员为启用范围保存配置 revision。`plugins.allowed` 为项目显式允许清单，缺省为空；初始化可按明确部署策略写入官方内置项，不能把“全局启用全部”当项目默认值。有效权限始终取交集，密钥只存引用，工作区输入不能修改入口、网络许可或资源上限。

### 7.2 有真实依赖需求时再实现

引入跨插件服务时，将服务定义、提供者和消费者分开，固定 application/project/node/invocation/web-view 作用域；短作用域不能被长作用域保存为全局可变状态。激活前解析 required/optional 依赖、版本、target 和冲突，缺必需依赖或有环就拒绝，可选缺失才允许显式降级。服务多提供者由配置选定，不实行“最后加载者获胜”。

该阶段才实现依赖锁定、反向传播、注册 handle 与幂等 dispose。必需提供者失效或撤销时阻止消费者新调用；更换版本或提供者必须生成新激活快照。ActivationSnapshot 从前期的包摘要、有效配置 revision 和启用策略 revision，扩展为包括依赖图摘要及能力版本；不重写旧快照。

多层配置也需明确需求才引入。拟采用包默认值→管理员→项目→节点/用户的可覆盖字段规则，安全策略始终独立取交集，不参与普通深合并。作用域、依赖服务、通用配置容器都不列入 M1/M2 的必交范围。

## 8. 控制数据与权威来源

M1 新接受的内置产物/证据必须由 registry 核验包身份与当前 `core_release_digest`，不能从外部自报 `builtin_release` 获权。旧 v1 证据缺少发布摘要时可按原字节读取和校验历史摘要，但不能用作新结果的可信来源证明；不自动补写历史记录。


| 数据 | 唯一来源/所有者 |
|---|---|
| PluginPackage / 包版本 / 清单 / 字节 | catalog/storage 的不可变配置资产；内置包绑定核心发布摘要 |
| 包审定 / Review / 检查证据 | ledger/provenance；登记表只存 review_id 引用或可核验投影 |
| Registration / Enablement / 策略 / 配置 revision | extensions 在 main 中的专属表；identity 提供授权，不让它代管插件生命周期 |
| ActivationSnapshot / 熔断；按需 Instance / 健康 / 部署进度 | extensions 在 runtime 中的专属表，随实际宿主分期建设 |
| TaskRun / AgentStepRun / JobAttempt | agent_execution/jobs；Task 的 Attempt/fence 仍唯一属于 tasks |
| 扩展事件 / 目录查询 | events / query 的派送与投影；不能裁决是否可启用 |
| 本地插件数据 | 宿主分配的私有命名空间目录或扩展自有存储；不是五库业务表 |
| 密钥 | 宿主管理的密钥设施；manifest/config 只存引用与用途 |

跨库用稳定 operation 与 outbox，不能一次 SQL 事务“同时批准、启用并启动”。外部包首次启用先验证 ledger 中当前有效审定，再由 main 保存启用意图，runtime 保存实际宿主所需的激活/故障状态。`enabled` 是期望状态；一次性插件按能力核验与逐次准入判定可用，常驻实例另有 `ready`。撤销审定后核心立即拒绝后续授权，已运行进程的停止由恢复协议推进。

M1 自举采用独立的 `builtin_release` 来源：只有绑定当前核心发布摘要的官方编译组件，才能由实例初始化/部署策略静态登记与激活；不伪造包 Review 或 HumanGrant，也不需要先启动 M2 的插件人审功能。随核心发布替换这些组件仍需正常发布/部署管理。所有独立安装的外部包，即使发布者为官方，也必须走 M2 的包审定和管理员启用，不得自报 builtin 绕过治理。

插件不能添加表到核心五库。插件自有状态必须声明 schema、保留期、备份和导入导出策略；升级前做兼容性与恢复预检。未经验证的降级不自动执行，回滚代码不等于回滚状态格式。

## 9. 权限与隔离

有效能力取交集：**包请求能力 ∩ 管理员启用范围 ∩ 调用主体权限 ∩ 本次任务/资源范围 ∩ 宿主能强制的约束**。UI 隐藏、客户端注解与包作者的声明均不能代替服务端检查。

所有调用记录 actor/session、plugin ID/版本/包摘要、activation generation、project、operation、输入摘要与结果；常驻实例含 instance ID，任务调用含 Attempt/fence。插件调用方身份与被委托用户身份分别记录，不能伪装成用户直接调用。失效 activation 不得借仍有效的普通会话提交结果。

| 执行形态 | 凭据与约束 |
|---|---|
| 一次性处理器 | 默认仅有输入文件与输出目录，无兰台令牌；由 worker 验收/提交。需密钥的特例声明用途，经批准由宿主短期注入或代理调用 |
| Agent adapter / 远程 service | broker 或服务端签发的短期、限 audience/项目/动作的服务凭据；不传用户长期令牌、人类证明或签名密钥 |
| 本机 CLI | 显式本机安装与信任；优先 broker，宿主控制 argv/env/cwd，服务端始终复验 |
| 网页插件 | 有界 broker，不发核心凭据；敏感动作由宿主页面完整办理 |

进程外运行改善崩溃与生命周期隔离，但不会自动限制同一 OS 用户的文件读取、网络或资源耗尽。宿主发布 `isolation_capabilities`，按平台实测 cgroup/进程树、文件和网络限制；达不到插件要求时拒绝执行或只允许明确受信任的部署策略，不能默默降级后宣称沙箱运行。

数据目录不挂给插件。安装包校验路径穿越、绝对路径、符号链接、大小写冲突和解压配额；安装步骤不执行包自带 postinstall。权限扩大、依赖变更或可执行内容变更，都需要重新核对摘要和启用授权。签名可证明包来源，不替代业务批准与隔离。

## 10. 生命周期与运行协议

### 10.1 启用意图与执行形态

包记录、作用域启用意图与运行观察分别保存。包审定实时引用 ledger，T09 不复制审批状态机。被撤销包保留历史记录，但不再派发，也不接受失效代次的结果。

| 形态 | 就绪与运行 | 宿主控制 |
|---|---|---|
| M1 官方内置组件 | 静态表通过校验后可调用 | 随核心发布和进程生命周期管理 |
| M2 一次性 processor / 命令 | 包已获准、入口/能力/配置可用；每次任务启动进程并退出 | 宿主负责超时、取消、退出和临时资源回收；无后台 Health/Stop RPC |
| 按需常驻实例 | resolved → starting → ready → draining → stopped，异常为 failed/quarantined | 实例专用握手、健康与运行控制协议 |

`enabled` 表示期望可用，不代表当前可派发。一次性插件可用性由包治理与静态/已授权能力核验决定，不要求保活进程或周期健康心跳。常驻实例才有进程级 ready/health；Health 成功也不等于业务任务完成。数据归属仍按第 8 节，按阶段增加实际需要的表，不为 M1 预建整套实例控制器。

### 10.2 一次性处理器：文件协议

1. 派发前核验包/入口摘要、启用范围、项目允许清单、调用授权、输入版本及任务 fence，冻结配置和 activation generation。
2. 宿主创建 `job.json`、只读 `in/` 与独立 `out/`。`job.json` 固定协议版本、扩展 ID/版本/包摘要、operation/attempt、输入摘要、参数、预期 schema 与 deadline；启动已登记入口，传入 job 文件路径。
3. 插件只在 `out/` 生成候选文件和 `out/result.json` 后退出。标准错误有界记录，日志不是协议；一次调用不启动 Describe/Negotiate/Configure/Health/Drain/Stop 服务。协议兼容性通过清单、任务和结果 schema 版本校验。
4. 退出码 0 仅表示程序正常结束；还必须存在完整且 schema 合法的结果。结果可含合法检查 `fail`，它属于业务不合格；崩溃、超时、缺失/畸形结果不能伪装为 pass。退出非 0 属运行失败，不作为表达合法检查 fail 的方式；输入不支持用 schema 规定的分类结果并正常退出，或由宿主派发前拒绝，不能靠崩溃来表达。
5. 宿主检查输出数量/大小/路径与摘要，拒绝越界、穿越、符号链接逃逸与未声明产物；worker/服务端再按领域契约接受。最终提交复验权限、包启用/代次与 fence，产物身份和证据可追溯。`result.json` 记录 `status`、`records`、`checks`、`files`，处理器/校验器为 `lantai.processor-result/v1`，本机命令为 `lantai.cli-command-result/v1`；校验器以只读 `in/manifest.yaml` 取得目标版本清单，`status: unsupported` 表达不支持的输入。
6. 超时或取消由宿主处理进程树、deadline 与目录回收，不依赖插件正确响应 Stop。可用 OS 约束按平台实测，不能承诺子进程天然隔离。

### 10.3 常驻实例：按需控制协议

只有实际建设常驻 service/受托管常驻后端时，才实现静态校验→权限/配置冻结→资源准备→握手→健康→发布贡献；有跨插件依赖时再加依赖解析。失败按逆序释放实际取得的资源，完成前不能显示 ready。

候选控制传输为有界 JSON-RPC/stdio，本机/三平台小样通过后再定；远程服务用经认证 HTTP。控制操作包括 Describe、Negotiate、Configure、Health、Drain、Stop，带 deadline/request_id/operation_id，日志与协议分流。握手校验版本、包/入口摘要、instance ID 和会话材料；格式标志串不作为身份认证。该协议不套在一次性处理器上，也不替代 T06 的 TaskRun/Job 业务协议。

Stop 区分宿主托管进程与外部共享服务/人工会话。后者默认仅解除兰台注册、授权及取消本次调用；没有独立管理授权不能终止共享服务。无法停止时明确报告能力不足/待对账。宿主掌握自己的路由、订阅和授权句柄，收回能力不依赖第三方正确清理。

### 10.4 共用撤权、升级与回执规则

正常升级停止派发旧版本新调用，可让已接受调用在限定旧代次与 deadline 内完成；一次性插件的排空就是等待这些调用结束，不向它发送 Drain RPC。权限撤销/安全隔离则立即失效能力与结果接受权，必要时由宿主取消进程。任务 fence 与 activation generation 均须通过。

每个有副作用调用有持久 intent/receipt 所有者：执行归 T06 Job/TaskRun，投递归 T04 delivery，其他动作归原命令模块；T09 只保存插件生命周期操作。固定 operation_id/request_hash，同键异摘要拒绝。未知效果进入 `needs_reconciliation`；协议不具备幂等或状态查询时不能换键、版本、实例盲目重试。业务幂等保留期覆盖允许重试期。

管理员手动启停走敏感命令授权；熔断与撤权按预授权策略自动收回能力。自动恢复不能重新启用已被手动停用、已撤权或已撤销审定的包。

## 11. 安装、升级与故障

### 11.1 导入和能力探测

以下针对独立安装外部包；M1 内置组件使用第 8 节发布信任规则。

1. 导入仅登记字节，校验单一清单、路径、摘要、schema、平台/权限和许可证据，不执行入口、postinstall 或 capability probe。自举用随核心发布的静态验证器，不让待审代码批准自己。
2. 包经有效审定，管理员给出绑定包摘要、target/范围、配置 revision 和 operation 的执行/启用授权；本机 CLI 另需本机信任。该授权可包含启用前的受控能力探测，不反过来用探测成功代替人审。
3. 无法静态确认的外部运行能力，才在授权内以无业务素材/长期密钥、限定工作目录、超时、网络与资源限制执行探测。结果固定包/入口/工具摘要和宿主环境，失败或撤权均不派发；工具、入口或环境变更使旧探测失效。M1 不实现通用探测服务；M2 的探测是 `mode: probe`、无业务输入的合成一次性调用，协议合法完成才可派发，环境摘要包括平台、宿主能力、核心发布摘要及包、入口与配置摘要。
4. 记录启用意图；一次性插件不启动常驻进程，常驻形态才由部署控制器启动并报告观察状态。重启只能恢复仍有效的策略，旧缓存不能重新授予权限。
5. 升级先核验新包并保留旧包，按第 10 节分流新旧调用；状态格式不兼容时拒绝自动回滚。历史 CheckResult/Review 不修改，仅按输入/配置/Profile 所要求的处理器版本计算 stale/unknown。
6. 包被任务、证据或备份引用时不得清理；卸载保留登记墓碑和历史证据。代码回滚不等于状态数据回滚。

### 11.2 M2 熔断默认值

以下是兰台的可配置初始策略，供 M2 精确测试，不是外部框架或行业标准：

| 配置 | 默认值 | 含义 |
|---|---:|---|
| `plugins.breaker_failures` | 5 | 窗口内达到 5 次运行故障即 open |
| `plugins.breaker_window_seconds` | 600 | 滚动 10 分钟，按故障完成时间 `(now-600s, now]` 计数 |
| `plugins.breaker_cooldown_seconds` | 900 | open 后至少 15 分钟才允许半开 |
| `plugins.breaker_half_open_max_calls` | 1 | 每个熔断键只允许一个并发试探 |

熔断键为包摘要、入口、target、宿主节点和启用作用域，避免一个节点/项目故障暂停所有部署。计数按唯一 invocation/attempt 去重，超时及其后进程退出只计一次。计入崩溃、超时、畸形/缺失输出及可归因该调用的启动/运行故障；**合法检查 fail、不支持输入、权限/配置拒绝、用户取消和未派发的排队失败不计**。普遍基础设施故障独立告警，不伪装为插件坏输入。

closed 时业务成功不清空尚在滚动窗内的故障；达到阈值原子转 open 并停止新派发，在途调用按已冻结授权/deadline 收尾，安全撤权另行立即失效。15 分钟到期只进入可试探状态，不自动启动业务作业。宿主原子获取一个 half-open 名额，使用下一次正常获授权调用，或获授权的无副作用合成探测（M2 实现只用前者）；禁止为了试探重放未知副作用。

试探正常完成且协议合法即关闭并清空窗口，检查结果为合法 fail 也算运行恢复；运行故障则重新 open 并开始新的 15 分钟。取消/权限拒绝等非故障不计成功；未派发或已确认结束才释放名额，已派发且停止状态未知则保留占位、先回收/对账，不能并发补发试探。每次派发固定熔断 epoch；转 open 或成功关闭并重置窗口时推进 epoch，半开试探领取当前 epoch。普通调用与试探的回调都复验所属 epoch，旧 epoch 的迟到成功/故障只留审计，不改变新窗口、冷却或状态；结果能否作为业务证据另按任务授权与代次判断。探测还须复验当前启用/代次，不能解除撤权或新一轮熔断。持久化窗口/状态/open 时间和试探租约，重启不丢失冷却或产生并发探针；无法确认旧探针是否结束时先回收/对账，不补发有副作用调用。参数修改留 revision，手动停用/审定撤销始终优先于自动恢复。

## 12. 必须具备的诊断与验收

管理接口给出当前形态的拒绝原因：不兼容、未支持能力、权限未批、配置无效、隔离不足、被熔断或撤销；依赖/常驻机制启用后再报告依赖缺失、健康失败。日志串联 operation/plugin/attempt，适用时带 instance；屏蔽凭据与敏感参数，不把插件日志当协议响应或操作指令。以下按阶段验收，不把按需项计入 M1/M2 必交任务。

| 阶段/场景 | 通过标准 |
|---|---|
| M1 单一清单与静态登记 | 旧/双清单、重复 ID、不支持 target/扩展点/非空插件依赖明确拒绝；内置组件和产物含可核验身份摘要 |
| M2 导入与 probe | 导入不执行；未获准不得探测；探测失败或过期不派发，不改变包审定事实 |
| M2 一次性调用 | 只用 job/输入/输出文件及退出，无常驻控制 RPC；合法 fail 与运行故障区分，结果接受复验授权/fence/代次 |
| M2 启用响应丢失/重启 | 同 operation 恢复同一意图，不重复未知副作用，不意外启动常驻处理器 |
| M2 停用/升级/撤权 | 宿主回收实际资源；正常升级可排空，撤权拒收旧结果；历史证据不改写 |
| M2 CLI 劫持 | 未登记同名程序不执行；入口摘要变更拒绝；不能遮蔽核心命令 |
| M2 熔断边界 | 600 秒窗口的第 4/5 次、超时重复回调、正常 fail、不计事件、900 秒冷却、单一半开、取消但停止未知、普通调用/探测跨 epoch 迟到结果及重启均按 §11.2 |
| M3 官方/声明式 UI | 插槽失败有后备，实际注册可清理，敏感动作由宿主办理；不要求第三方 iframe |
| 按需跨插件依赖/常驻服务 | 缺依赖、循环、冲突、依赖传播、控制握手/健康及失败清理有真实双端场景 |
| 按需第三方 UI | 独立 origin 和主站跨站部署验证；凭据、Cookie/CSRF、伪造消息、导航/撤权/跨包隔离均通过后才放行 |

## 13. 分期与目录

| 阶段 | 必须完成 | 保留到后续 |
|---|---|---|
| M1 | 服务端/节点处理器与校验器清单 schema、扩展点 ID 表、官方内置静态登记、产物/证据身份；CLI/Web 仅字段预留 | DI、插件依赖图、多层配置、通用卸载/常驻控制器与动态加载均不实现 |
| M2 | 一个一次性进程外检查插件、手动 Agent adapter、包审定启停、受控探测/执行、熔断、显式 CLI 与最小 SDK | 不强制常驻控制协议、自动 Runner 或通用服务容器 |
| M3 | 首方网页插槽与声明式设置、选定自动执行后端和受控触发 | 第三方 iframe 不属该阶段承诺；后端实际需要常驻能力时单独拆卡 |
| 按需 | 跨插件服务依赖/DI、常驻宿主；或独立来源第三方 UI，分别有真实需求、部署方案与验证任务 | 未立项前保持未支持，不因预留而自动进入 M3 |
| M4–M8 | 导入导出、更多查看器/标注、检索服务、推送连接器和官方存储驱动 | WebAssembly 沙箱及公开市场按真实需求另定 |

保持 11 个一级目录。`plugins/` 承载官方扩展包源码及分 target 示例，`internal/` 放服务端 registry/manager/broker/host，`cmd/` 只组装入口，`web/` 放 UI registry/broker/首方组件，`schemas/` 放协议，`sdk/` 放客户端和插件 SDK，`tests/` 放兼容性与故障场景。M1 已建立 `internal/extensions`、`schemas/extensions` 与 `plugins/corecheck`；未启用阶段不预建通用管理器、broker 或进程容器。

后续 `lantai plugin` 用于包治理；`lantai ext` 用于调用已启用的客户端贡献。两者是同一扩展身份体系的管理面与执行面，不维护两套插件库。
