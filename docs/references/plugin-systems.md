# 插件系统参考评估

核对日期：2026-09-26。用途：支撑[三端扩展设计](../extensions.md)的机制选择，不代表引入依赖、兼容外部插件格式或完成安全验证。在线资料为当日官方文档；除 DeepSeek Harness 外未锁发行版，真正采用库时由 T00 锁 tag/commit 与许可证。

## 评估结论

兰台适合组合借鉴这些项目的机制：统一清单和静态贡献、窄服务接口、可撤销注册、可观测宿主、独立网页扩展制品。它们分别解决组装、分发和故障管理；权限、业务事实与不可信代码隔离仍需兰台自行落实。

| 参考项目 | 已核实机制 | 采用到兰台 | 不直接采用 |
|---|---|---|---|
| DeepSeek Harness / Cordis | 服务 DI、依赖生命周期、effect 清理、包/运行分层和 UI slots | 定义/提供者/消费者分离；注册有所有者；迟到运行消息拒绝 | 任意动态 JS、单 operator 权限模型、把 DI 当沙箱 |
| VS Code | manifest、贡献点、按需激活、不同运行宿主 | CLI 命令、设置、视图的有限注册及兼容检查 | 高权限本机宿主作为安全边界；完整 Marketplace |
| HashiCorp go-plugin | 本机子进程、RPC、协议协商和生命周期 | 作为进程外传输候选；Lantai 契约包在其上 | 直接用于远程网络服务；cookie 当授权 |
| Caddy | 编译注册与 Provision/Validate/Cleanup | 官方可信模块、配置验证与清理 | 任意 Go 动态库；配置验证触发业务副作用 |
| Backstage | 服务作用域、窄扩展点、typed UI inputs/outputs | 宿主 API 与网页插槽 | 引入整套平台；同源插件当隔离执行 |
| JupyterLab | prebuilt、token DI、设置 schema、依赖共享 | 独立网页制品、接口包与配置契约 | 把 Module Federation、服务 token 当安全权限 |
| Grafana | backend 能力接口、健康、资源路由、协议版本 | 能力就绪、受控资源 API、版本分别管理 | 只在内存保存业务恢复事实；插件路由绕过核心授权 |

下文是来源事实与限制，具体的三端权限、五库归属和分期是兰台设计推导。

## DeepSeek Harness：固定源码证据

只读核验用户指定的本地 checkout，提交为 `477b4f420553e8a52c2fbccc464d7561b239c443`；阅读前后工作区均干净。远端确认为官方 [deepseek-ai/deepseek-harness](https://github.com/deepseek-ai/deepseek-harness)。未启动、安装或执行其测试。

| 证据 | 固定源码 | 含义 |
|---|---|---|
| 服务登记与撤回 | [service.ts](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/vendor/cordis/src/service.ts#L32)、[reflect.ts](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/vendor/cordis/src/reflect.ts#L277) | 同作用域重复服务拒绝；撤回通知依赖 |
| 清理与依赖 epoch | [fiber.ts](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/vendor/cordis/src/fiber.ts#L403) | effect 内清理项逆序；fiber 顶层等待清理，不是全局严格逆序 |
| 包与运行身份 | [registry.ts](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/extensions/cordis-host-runner/src/registry.ts#L16)、[运行撤回](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/extensions/cordis-host-runner/src/index.ts#L893) | plugin/package/run 分开，旧 run 不能继续调用 |
| UI slot 所有权 | [ui-slots](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/client/ui-slots/src/index.ts#L100)、[registry](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/client/ui-renderer/src/client/registry.ts#L195) | 声明 kind/scope/owner；贡献随所有者回收 |
| 执行环境限制 | [Node evaluator](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/extensions/cordis-host-runner/src/sandbox.ts#L68)、[Browser evaluator](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/extensions/cordis-client-runner/src/client/evaluator.ts#L166) | vm / new Function 不能作为独立 OS/origin 安全边界 |
| 管理入口与补偿 | [CLI](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/apps/cli/src/plugin.ts#L10)、[包操作](https://github.com/deepseek-ai/deepseek-harness/blob/477b4f420553e8a52c2fbccc464d7561b239c443/packages/boot/plugin-manager/src/operations.ts#L465) | 共用包管理操作值得借鉴；不能据此承诺所有安装/升级失败无损回滚 |

按实际资源引入其生命周期思想；M1 不建设通用 DI/依赖容器，保留兰台自己的授权、任务 fence 与持久回执。源码中的依赖 token 表示服务需求，不等于用户授予权限；动态 Host/Browser 激活也不能直接套用到多项目协作。

## 服务端官方来源

- **HashiCorp go-plugin**：[官方说明](https://github.com/hashicorp/go-plugin)、[内部握手](https://github.com/hashicorp/go-plugin/blob/main/docs/internals.md)、[协议版本与 cookie](https://github.com/hashicorp/go-plugin/blob/main/server.go)、[启动与清理](https://github.com/hashicorp/go-plugin/blob/main/client.go)。支持本机进程外 RPC；magic cookie 是误启动保护，官方不将其定位成远程协议。宿主仍负责业务幂等、授权和 OS 限制。
- **Caddy**：[模块生命周期](https://caddyserver.com/docs/extending-caddy)、[自定义构建](https://caddyserver.com/docs/build)。可信模块编进二进制，配置阶段构造、准备、校验并清理；新旧实例可能重叠。兰台不能因配置校验或重新准备而重复创建业务作业。
- **Grafana**：[backend 能力](https://grafana.com/developers/plugin-tools/key-concepts/backend-plugins)、[生命周期](https://grafana.com/developers/plugin-tools/key-concepts/plugin-lifecycle)、[协议版本](https://grafana.com/developers/plugin-tools/key-concepts/backend-plugins/plugin-protocol)、[资源处理](https://grafana.com/developers/plugin-tools/how-to-guides/app-plugins/add-resource-handler)。能力与健康接口、协议版本独立管理值得借鉴；资源路由仍需要逐动作鉴权和副作用判断。
- **Backstage**：[services](https://backstage.io/docs/backend-system/architecture/services/)、[extension points](https://backstage.io/docs/backend-system/architecture/extension-points/)、[modules](https://backstage.io/docs/backend-system/architecture/modules/)。显式依赖和作用域适合组织窄接口；兰台采用接口思想，无需为 Go 核心引入另一套后端平台。

## 客户端与网页官方来源

- **VS Code**：[manifest](https://raw.githubusercontent.com/microsoft/vscode-docs/main/api/references/extension-manifest.md)、[contribution points](https://code.visualstudio.com/api/references/contribution-points)、[activation](https://code.visualstudio.com/api/references/activation-events)、[extension host](https://code.visualstudio.com/api/advanced-topics/extension-host)。先声明后激活、按宿主选择能力；[运行安全说明](https://code.visualstudio.com/docs/configure/extensions/extension-runtime-security)指出普通扩展宿主具有与应用相同的权限。有限 API 不等于恶意本机代码无法读取用户文件。
- **JupyterLab**：[prebuilt / tokens / settings](https://jupyterlab.readthedocs.io/en/stable/extension/extension_dev.html)、[安装风险与兼容](https://jupyterlab.readthedocs.io/en/stable/user/extensions.html)。本次 stable 页面显示 4.6.4；独立制品、类型 token 和 schema 改善分发与组合，管理器不保证安装项兼容。仅借用机制，不锁该版本作为依赖。
- **Backstage frontend system**：[typed extensions](https://backstage.io/docs/frontend-system/architecture/extensions/)、[blueprints](https://backstage.io/docs/frontend-system/architecture/extension-blueprints/)、[utility APIs](https://backstage.io/docs/frontend-system/architecture/utility-apis/)。可将页面、动作、预览收敛为窄创建接口；[threat model](https://backstage.io/docs/overview/threat-model/)明确不负责沙箱化插件，因此仅适合作为可信组件组装参考。
- **浏览器规范**：[iframe sandbox](https://html.spec.whatwg.org/multipage/iframe-embed-object.html#attr-iframe-sandbox)、[Web messaging](https://html.spec.whatwg.org/multipage/web-messaging.html#security)、[Fetch](https://fetch.spec.whatwg.org/#fetch-method)。同源 iframe 组合开放脚本和同源权限存在逃逸风险；消息核验来源与结构，敏感消息不能用通配目标。Worker 无 DOM 不等于没有网络能力。兰台的独立来源与 CSRF/broker 方案必须以目标浏览器实测为准。

## 采用前仍需验证

1. M2 验证一次性处理器的三平台进程控制、文件协议、隔离能力与清理；常驻宿主有真实需求时才验证控制协议 framing、stdio 或其他本机 RPC 实现。
2. M1 用官方静态 registry 验证契约，M2 一个真实处理插件验证全链路；不为所有预留扩展点预建服务。
3. M3 仅首方/声明式网页。第三方代码按需单独立项，再验证独立 origin、与主站跨站的部署、Cookie/CSRF、跨插件通信及撤权；不满足条件就保持首方/声明式扩展。
4. 包、协议、配置/状态 schema 与业务输入版本分别锁定；各候选库的版本、许可证、维护成本在实际采用时另记录 ADR。

## v2.1 评审补充

2026-09-26 再核对 [Chrome 对 same-site / same-origin 的说明](https://web.dev/articles/same-site-same-origin)和 [WHATWG Web messaging](https://html.spec.whatwg.org/multipage/web-messaging.html#security)：不同端口/子域可形成不同 origin，但同 scheme、同可注册域仍可能同站。来源隔离不能单独防止携带主站会话的写请求；消息必须校验来源与结构，敏感投递不能用通配目标。

由此采用的项目取舍见唯一[实现规格](../extensions.md)：第三方网页按需，跨站部署仍保留 Origin/CSRF 和 broker 校验；一次性处理器沿文件协议，常驻 RPC 不列入 M2；10 分钟 5 次运行故障、15 分钟冷却及单探针是兰台可配置初值，不是上述项目或规范的推荐常数。能力 probe 是代码执行，不能混入未经授权的静态导入。
