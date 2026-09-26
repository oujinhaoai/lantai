# 服务端、客户端与网页扩展设计 v2

状态：**设计补全，待实现验证**。本篇细化插件机制，不启动开发、安装插件或启用自动化。现行目录、Go 单核心进程、五库和核心裁决规则保持不变。参考证据见[插件系统调研](references/plugin-systems.md)，实施拆分见 [T09 扩展平台](tasks/T09-extension-platform.md)。

## 1. 评估结论

现有方案适合启动文件处理插件，但还不足以支撑三端扩展。缺口主要是：扩展点目录及版本、依赖与服务发现、统一生命周期、客户端安全发现、网页插槽与宿主通信、插件治理的数据归属，以及升级后在途任务怎么继续。

采用**统一扩展包与注册规则，分端宿主执行**：

| 层次 | 做什么 | 实现方向 |
|---|---|---|
| 共同控制层 | 包身份、摘要、版本、贡献声明、权限、启用范围、依赖、配置、兼容性 | 核心 `extensions` 模块与版本化 schema；客户端仅缓存已授权视图 |
| 服务端/节点宿主 | 处理器、校验器、执行后端、连接器、外部能力服务 | 进程外协议；正确性关键驱动仍随官方核心编译发布 |
| 客户端宿主 | 显式安装的子命令、导入导出、DCC 适配和受控 MCP 工具贡献 | 本机注册表、受控进程和薄 SDK，不扫描工作目录后自动执行 |
| 网页宿主 | 预览、侧栏、动作、设置、页面、任务结果视图 | 首方组件注册；第三方代码隔离加载并经有限 broker 通信 |

核心只提供明确的接口，不引入一个“任意插件都能替换任何模块”的总线。权限最终判断、稳定 ID、提交、台账审定/发布/清除和 Task 租约状态机不能被运行时插件覆盖。业务扩展通过配置、异步任务和候选证据参与这些流程。

## 2. 借鉴与取舍

- DeepSeek Harness / Cordis：借鉴服务定义、提供者与消费者分离，以及所有注册可撤销的生命周期；不照搬“所有核心逻辑都可以被运行时替换”。
- VS Code：借鉴静态 contribution manifest、能力/宿主声明、按需激活与客户端命令注册；extension host 不自动等于安全沙箱。
- HashiCorp go-plugin：借鉴进程外握手、协议版本和宿主回收；首版保持语言中立，不以其具体 RPC 栈作为硬依赖。
- Caddy / Backstage：借鉴命名空间、初始化/验证/清理，以及显式依赖注入与可扩展接口；官方编译模块和外部不可信代码分开治理。
- JupyterLab / Grafana：借鉴可组合视图、设置 schema、面板/数据源和前后端协同；模块联邦或远程 React 组件本身不能构成安全隔离。

这些是兰台对机制的选择，并非宣布兼容上述项目的插件格式。来源、版本范围和具体限制集中记录在[调研文档](references/plugin-systems.md)。

## 3. 服务端需要预留的扩展点

“预留”要求现在确定名称、输入/输出、权限及所有者；具体宿主按阶段实现。未支持的扩展点明确返回 `EXTENSION_POINT_UNSUPPORTED`，不悄悄忽略配置。

| 扩展点 | 输入 → 输出 | 谁验收/裁决 | 实现阶段 |
|---|---|---|---|
| `asset.type` | 类型/schema/文件角色 → 校验后的类型配置 | catalog，配置固定版本 | M1 声明式 |
| `metadata.extractor` | 固定 manifest → 结构化元数据与证据 | provenance；更新须走条件命令 | M1 内置，M3 外部 |
| `asset.validator` | 固定版本+Profile → CheckResult | ledger/Profile，插件只提供结果 | M2 |
| `artifact.processor` | 获授权输入 → 派生文件/记录 | jobs 验收，再走普通入藏 | M2 最小，M3/M5 扩充 |
| `ingest.importer` | 选定外部输入 → 候选 manifest/来源映射 | catalog/ledger，支持 dry-run | M3/M4 |
| `export.connector` | 固定版本+用途 → 外部传输回执 | 核心先检查许可和导出权限 | 按需求，默认关闭 |
| `search.provider` | 获授权查询/候选 → 排名与扩展字段 | query 在输入和输出均检查权限 | M6；M1 仅接口预留 |
| `workflow.template` | 声明式步骤和依赖 → 固定 FlowDefinition | workflow 解析与版本锁定 | M2 固定，M3 扩展 |
| `agent.backend` | TaskRun/输入锁/预算 → 状态、检查点与候选 | agent_execution + tasks + ledger | M2 手动，M3 后端 |
| `notification.channel` | 已授权事件投影 → 发送回执 | events 持久出站、幂等和脱敏 | M7 |
| `event.consumer` | 按声明过滤的事件 → ack/后续命令 | 各命令所属模块再次授权 | M3+，先内部消费 |
| `service.api` | 已登记路由/schema → 能力服务结果 | 核心鉴权、配额、请求/响应校验 | 具体能力需要时 |
| `storage.driver` / `file.install` / `db.driver` | 官方接口 → 持久化操作 | 核心及平台测试 | M1 内置，后续编译扩展 |

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

禁止“找不到命令就执行 PATH 中任意同名程序”。显式安装时记录完整解析路径、平台和摘要，调用前核验；工作目录中的同名文件不能劫持命令。包来源、argv 模板和入口都来自已登记元数据，不将任务说明拼接为 shell 命令。

启动环境使用白名单，默认不继承宿主密钥和完整环境变量。一次运行得到限定的工作目录与能力通道；访问核心优先经 broker，确需直接连接时由服务端签发短期、限项目/用途/插件的凭据，不传长期用户令牌。能运行本地代码仍意味着本机信任，不能把 CLI 父子进程关系称作沙箱。

MCP 不开放人审、凭据管理、提前清除、插件启用或任意 shell。插件不得仅凭标注 `readOnly` 获得权限；行为注解由已验证命令属性生成。读取任意外部文件和联网等能力也需单独声明并受宿主策略限制。

## 5. 网页扩展与插槽

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

隔离部署可复用同一网关端口，用专用 HTTPS origin 路由静态插件包。不互信的包及不同权限实例不能共用 origin；origin 绑定到包摘要/实例策略，停用或升级按策略清除存储、缓存和 Service Worker，不向新实例复用旧能力。主站 Cookie 不设置跨 origin 共享域，插件 origin 不承载业务会话。

首版 broker 固定使用已登记独立 HTTPS origin，iframe 仅开放 `allow-scripts allow-same-origin`，其余 sandbox 能力默认关闭；这里的来源必须与宿主和其他不互信插件分离。拒绝 `null` 或通配来源，不把 opaque-origin iframe 混入此模式。CSP、frame-ancestors 与 Permissions-Policy 默认拒绝、逐能力放行；三种目标浏览器均须实测。

host-only Cookie 不能单独防止插件诱导浏览器携带主站登录态发请求。业务 API 不允许扩展 origin 的带凭据 CORS；Cookie 写请求验证准确 Origin 与 CSRF，GET 无副作用；插件动作只经 broker。来源隔离、服务器请求校验与 broker 授权共同成立后才开放第三方 UI。

broker 只接受登记动作，逐条校验 schema、请求 ID、插件/激活代次、对象范围和限额。建立通道时核对准确 origin、窗口 source 与握手 nonce；不用 `*` 投递敏感数据。页面导航、注销、切项目、撤权和插件停用都关闭旧通道、取消订阅并丢弃迟到响应。人审动作需要宿主真实用户操作、服务端冻结目标及独立验证，子页面消息不能直接换取人审授权。

浏览器机制依据：[iframe sandbox](https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Elements/iframe#sandbox)、[postMessage 的来源校验](https://developer.mozilla.org/en-US/docs/Web/API/Window/postMessage#security_concerns)。具体 broker、权限与代次规则是兰台的设计。

## 6. 统一扩展包

一个发布包可包含多个 target，但各 target 独立审查权限、激活与故障处理；服务器启用不等于在用户电脑或网页自动执行。以下是设计示例，尚无可用解析器：

```yaml
contract: lantai.extension/v1
id: org.example.asset-tools
version: 0.1.0
compatibility:
  host_api: ">=1.0.0 <2.0.0"   # 示例范围，实际宿主版本在 T00 固定
  required_points:
    - { id: asset.validator, version: 1 }
contributes:
  - id: org.example.asset-tools.check
    point: asset.validator
    target: node
    input_schema: lantai.check-input/v1
    output_schema: lantai.check-result/v1
  - id: org.example.asset-tools.inspect
    point: cli.command
    target: cli
targets:
  node: { runtime: exec, entry: bin/checker, protocol: lantai.processor/v1 }
  cli: { runtime: exec, entry: bin/inspect, protocol: lantai.command/v1 }
permissions:
  api: ["assets:read", "checks:submit"]
  network: []
  filesystem: ["granted-input:read", "run-output:write"]
  secret_refs: []
dependencies: []
config_schema: schemas/config.json
```

真实包清单还必须列出平台产物与摘要、配置/状态 schema 版本、资源上限、许可归属、所需宿主能力与可选能力。包 ID、展示名、贡献 ID 分开；所有贡献和服务 token 有命名空间。同版本不同摘要禁止覆盖，依赖锁文件固定每个包的摘要与服务提供者。

分发单元统一，调用协议按类型区分：声明式配置、`processor` 一次性文件协议、Agent adapter 的 TaskRun 控制协议、常驻 service API、CLI 命令协议、web broker。统一治理不要求把所有类型塞进同一个 `run()`。

## 7. 服务定义、依赖与作用域

扩展接口分三种角色：服务定义负责版本与 schema，提供者实现能力，消费者只依赖服务定义。核心 Go 模块用受控组装；外部提供者使用语言中立的协议代理。提供者可以替换，不代表能替换权限或事务规则。

作用域固定为 application、project、node、invocation 和 web-view。短作用域不能被长作用域保留成全局可变对象。每次调用的主体、项目、用途、输入锁和 deadline 显式传入；不从可变全局“当前用户”推断。

激活前先解析 required/optional dependencies、版本与 target 可用性：必需依赖缺失/循环/不兼容则拒绝；可选依赖不具备时显式降级。单一服务多个提供者由配置选定，多贡献槽按稳定优先级加 ID 排序，重复 ID 拒绝，不实行“最后加载者获胜”。

运行中必需提供者失效、撤销或熔断，沿反向依赖图阻止消费者新调用并转 degraded/draining；在途调用按各自授权收尾或取消。可选依赖按声明降级。更换提供者或其版本须重新生成激活快照，不能不留记录地换绑仍在运行的消费者。

冻结的 ActivationSnapshot 包含包摘要、依赖图摘要、配置 revision、启用策略 revision 与各能力的版本。M1 只需支持官方静态提供者，先让扩展点可测试；不用先建设全功能动态依赖容器。

配置按 schema 声明允许的层次：包默认值 → 实例/管理员 → 项目 → 节点/用户偏好；后层只能覆盖声明可覆盖的字段，安全策略单独取交集，不参与普通深合并。工作区配置与任务输入不能改入口、网络许可、密钥来源或资源上限。全局可用不代表所有项目自动启用，项目采用明确允许清单；密钥只存引用，变更配置生成新 revision 并重新验证。

## 8. 控制数据与权威来源

| 数据 | 唯一来源/所有者 |
|---|---|
| PluginPackage / 包版本 / 清单 / 字节 | catalog/storage 的不可变配置资产；内置包绑定核心发布摘要 |
| 包审定 / Review / 检查证据 | ledger/provenance；登记表只存 review_id 引用或可核验投影 |
| Registration / Enablement / 策略 / 配置 revision | extensions 在 main 中的专属表；identity 提供授权，不让它代管插件生命周期 |
| Instance / ActivationSnapshot / 健康 / 熔断 / 部署进度 | extensions 在 runtime 中的专属表 |
| TaskRun / AgentStepRun / JobAttempt | agent_execution/jobs；Task 的 Attempt/fence 仍唯一属于 tasks |
| 扩展事件 / 目录查询 | events / query 的派送与投影；不能裁决是否可启用 |
| 本地插件数据 | 宿主分配的私有命名空间目录或扩展自有存储；不是五库业务表 |
| 密钥 | 宿主管理的密钥设施；manifest/config 只存引用与用途 |

跨库用稳定 operation 与 outbox，不能一次 SQL 事务“同时批准、启用并启动”。外部包首次启用先验证 ledger 中当前有效审定，再由 main 保存启用意图，runtime 启动后记录实际状态；`enabled` 是期望状态，`ready` 才表示运行就绪。撤销审定后核心立即拒绝后续授权，停止进程由恢复协议推进。

M1 自举采用独立的 `builtin_release` 来源：只有绑定当前核心发布摘要的官方编译组件，才能由实例初始化/部署策略静态登记与激活；不伪造包 Review 或 HumanGrant，也不需要先启动 M2 的插件人审功能。随核心发布替换这些组件仍需正常发布/部署管理。所有独立安装的外部包，即使发布者为官方，也必须走 M2 的包审定和管理员启用，不得自报 builtin 绕过治理。

插件不能添加表到核心五库。插件自有状态必须声明 schema、保留期、备份和导入导出策略；升级前做兼容性与恢复预检。未经验证的降级不自动执行，回滚代码不等于回滚状态格式。

## 9. 权限与隔离

有效能力取交集：**包请求能力 ∩ 管理员启用范围 ∩ 调用主体权限 ∩ 本次任务/资源范围 ∩ 宿主能强制的约束**。UI 隐藏、客户端注解与包作者的声明均不能代替服务端检查。

所有调用记录 actor/session、plugin ID/摘要、instance/activation generation、project、operation、输入摘要与结果；必要时含 Task Attempt/fence。插件调用方身份与被委托用户身份分别记录，不能伪装成用户直接调用。失效 activation 不得借仍有效的普通会话提交结果。

| 执行形态 | 凭据与约束 |
|---|---|
| 一次性处理器 | 默认仅有输入文件与输出目录，无兰台令牌；由 worker 验收/提交。需密钥的特例声明用途，经批准由宿主短期注入或代理调用 |
| Agent adapter / 远程 service | broker 或服务端签发的短期、限 audience/项目/动作的服务凭据；不传用户长期令牌、人类证明或签名密钥 |
| 本机 CLI | 显式本机安装与信任；优先 broker，宿主控制 argv/env/cwd，服务端始终复验 |
| 网页插件 | 有界 broker，不发核心凭据；敏感动作由宿主页面完整办理 |

进程外运行改善崩溃与生命周期隔离，但不会自动限制同一 OS 用户的文件读取、网络或资源耗尽。宿主发布 `isolation_capabilities`，按平台实测 cgroup/进程树、文件和网络限制；达不到插件要求时拒绝执行或只允许明确受信任的部署策略，不能默默降级后宣称沙箱运行。

数据目录不挂给插件。安装包校验路径穿越、绝对路径、符号链接、大小写冲突和解压配额；安装步骤不执行包自带 postinstall。权限扩大、依赖变更或可执行内容变更，都需要重新核对摘要和启用授权。签名可证明包来源，不替代业务批准与隔离。

## 10. 生命周期与运行协议

包记录、每个作用域的启用意图与运行观察分别保存：

```text
package: discovered → validated（审定结论实时引用 ledger，不复制状态机）
enablement[project/node/target]: disabled ↔ enabled（desired state）
instance: resolved → starting → ready → draining → stopped
                                  ↘ failed / quarantined
```

同一包可在不同范围具有不同启用状态。被撤销包保留历史记录，但当前 ledger/policy 判定不通过时一律不派发，不能等待观察状态变更才停止授权。

启动顺序为依赖解析 → 静态校验 → 权限/配置冻结 → 资源准备 → 握手 → 健康探测 → 发布贡献。任何步骤失败，按逆序释放已取得资源；完成前不向用户显示可调用状态。

所有路由、命令、订阅、计时器、后台循环、网页贡献和 broker channel 都由宿主持有注册 handle，并有幂等 dispose。停用先撤销新调用和派发，再按策略 drain/cancel 在途运行，最后反向清理；不能仅从列表删除一个名字。核心无需靠第三方正确执行 dispose 才能收回自己的路由、句柄和授权。

首版控制协议采用有长度边界的 JSON-RPC/stdio 候选；长期远程服务用经认证 HTTP。请求有 deadline/request_id/operation_id，限制消息大小并把日志与协议流分离；大文件用受控输入目录或授权资源通道，不通过 RPC 内联。选型由 T00/T09 的三平台小样最终固定。

控制面统一 Describe、Negotiate、Configure、Health、Drain、Stop；具体业务沿用 processor/Agent/service 协议，不能用 Health 返回成功替代 Ready 或任务完成。进程启动握手含双方协议版本、包/入口摘要、instance ID 和随机会话材料；格式检查用的标志串不是身份认证。

Stop 区分宿主拥有的托管进程与外部已存在的服务/人工会话。后者默认只解除兰台注册、授权及取消本次调用；没有独立管理授权时不能终止共享服务或人工启动的会话。无法实际停止时明确返回能力不足/待对账。

每个有副作用调用必须有持久 intent/receipt 所有者：执行由 T06 的 Job/TaskRun 管理，投递由 T04 delivery 管理，其他领域动作归原命令模块；T09 只保存插件生命周期操作，通用 service.api 不能绕过这些回执。请求固定 operation_id/request_hash，同键异摘要拒绝；协议声明 Lookup/status、取消、幂等保留期和恢复能力。幂等保留期必须覆盖允许的重试期，无法幂等或查询的模糊结果进入待对账，不能换实例、版本或新键重发。

停用有两种命令：正常升级可允许已接受调用在限定旧代次下完成；权限撤销/安全隔离立即失效能力与结果接受权。任务 fence 与插件 activation generation 都要通过。外部效果未知时进入 needs_reconciliation，禁止盲目换实例重试。

管理员手动启停采用敏感命令授权；健康熔断、依赖失效和撤权触发的自动隔离按预先授权策略执行，不等待新的 HumanGrant。自动恢复不能重新启用已被人停用或撤销审定的包。

## 11. 安装、升级与故障

以下流程针对独立安装的外部包；随核心编译的内置组件采用第 8 节的发布信任规则。

1. 导入包只登记元数据和字节；不执行代码。自举检查使用随核心发布的最小 manifest/hash/schema 验证器，不能要求先运行尚未批准的插件来批准自己。
2. 核对依赖、平台、权限、隔离要求及许可证据，提交明确版本/摘要给人审；管理员凭绑定包摘要、配置 revision、目标范围与 operation 的 HumanGrant 启用，停用也走对应敏感命令。客户端还需本机授权，网页只激活当前用户有权使用的贡献。
3. 启用写 desired state；通过部署控制器重试启动并报告 actual state。重启仅恢复仍有效的启用策略，不能靠旧缓存重新授权。
4. 升级先验证新版本并保留旧版本；新运行绑定新摘要，既有任务继续引用其固定版本或显式迁移。依赖图、配置和权限变化都有独立审计。
5. 插件升级不改写旧 CheckResult/Review。只有输入、配置或 Profile 所要求的处理器版本改变时，派生适用性变 stale/unknown，并按策略安排重算；旧记录仍可复现。
6. 熔断按包摘要/入口/target 统计基础设施失败、崩溃和超时；正常校验 fail 不是插件故障。阈值可配置，先定义时间窗、半开探测和恢复条件，不采用“遇到五个坏文件就停插件”的规则。
7. 无人引用旧包前才清理缓存；任务、证据和备份中的固定版本引用计入保留根。卸载保留注册墓碑与历史证据，不能删掉仍需恢复的包或状态。

## 12. 必须具备的诊断与验收

管理接口给出“为什么未激活”：不兼容、依赖缺失、权限未批、配置无效、隔离不足、健康失败、被熔断、被撤销。日志串联 operation/plugin/instance/attempt，屏蔽凭据与敏感参数；禁止把插件日志当协议响应或操作指令。

| 场景 | 通过标准 |
|---|---|
| 依赖缺失/环/提供者冲突 | 激活前明确拒绝，原有可用插件不被部分注册污染 |
| 相同命令/路由/视图重复注册 | 可预测冲突或稳定选择，无加载顺序覆盖核心 |
| 启用响应丢失/重启 | 同 operation 恢复到同一 desired state，不重复启动无幂等保护的工作 |
| 卸载后重装 | 无遗留路由、菜单、订阅、后台循环或旧 broker 句柄 |
| 撤权、切项目、退出登录 | 三端新调用拒绝，旧结果按 generation/fence 拒收，敏感缓存清掉 |
| CLI PATH/工作区劫持 | 未登记同名程序不会执行；改变已登记入口摘要后拒绝启动 |
| UI 伪造来源/消息/目标 | broker 拒绝；第三方 frame 看不到主站凭据/DOM；不能领取 HumanGrant |
| 插件崩溃/卡死/畸形输出 | 受影响调用失败、限额有效；核心已提交事实不改变，其他能力继续可用 |
| 插件升级/回滚 | 新旧运行固定各自版本；状态格式不兼容明确阻止；旧审定证据不被重写 |
| 处理器返回合法 fail | 进入验收失败，不误触发崩溃熔断；缺少结果与未知状态不算 pass |

## 13. 分期与目录

| 阶段 | 必须完成 | 保留到后续 |
|---|---|---|
| M1 | 三端 manifest/扩展点/服务定义、包版本与启用模型、官方静态注册与 dispose 机制、兼容性反例、协议与隔离小样 | 不自动下载第三方代码，不运行完整网页/插件市场 |
| M2 | 一个进程外检查插件、手动 Agent adapter、包审定启停、健康/熔断、CLI 显式命令与最小插件 SDK | 不强制自动 Runner，不实现所有预留扩展点 |
| M3 | 首方网页插槽与设置、自动执行后端；隔离验证通过后接第三方 iframe；包分发/缓存与受控触发 | 能力服务按实际需求实现，不为每个扩展点部署服务 |
| M4–M8 | 导入导出、更多查看器/标注、检索服务、推送连接器和官方存储驱动 | WebAssembly 沙箱及公开市场按真实需求另定 |

保持 11 个一级目录。`plugins/` 承载官方扩展包源码及分 target 示例，`internal/` 放服务端 registry/manager/broker/host，`cmd/` 只组装入口，`web/` 放 UI registry/broker/首方组件，`schemas/` 放协议，`sdk/` 放客户端和插件 SDK，`tests/` 放兼容性与故障场景。当前只更新文档，不预建这些二级实现目录。

`lantai plugin` 用于包治理；`lantai ext` 用于调用已启用的客户端贡献。两者是同一扩展身份体系的管理面与执行面，不维护两套插件库。
