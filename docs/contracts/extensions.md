# M1 扩展清单与内置登记契约

实现规格仍以[扩展设计](../extensions.md)为唯一权威。本页记录 `internal/extensions` 的可调用接口、摘要算法与 M1 验证边界；机器字段由 [extension.schema.json](../../schemas/extensions/v1/extension.schema.json) 和[公共来源引用](../../schemas/common/v1/defs.schema.json)定义。

## 清单与包身份

包根只有 `extension.yaml`，契约为 `lantai.extension/v1`，支持受限 YAML 或 JSON。`Parse` 只检查形状；`CheckM1` 再检查兼容性、扩展点及阶段能力。M1 支持的 host API 声明是 `1.0.0` 或 `>=1.0.0 <2.0.0`，不把未知范围当作兼容。CLI/Web target 和插件 dependencies 保留形状，登记时明确拒绝；其他未支持扩展点、外部 runtime、未登记本机工具或未知输出 schema 同样返回 `EXTENSION_POINT_UNSUPPORTED`。

`ReadPackage(fs.FS)` 静态核对整个文件集。包内任何文件都必须声明，唯一例外是清单本身；清单的 `files` 不列自身摘要。入口须在平台制品表中，且制品与文件表的路径、大小及 SHA256 一致。配置 schema 必须能离线编译，许可证据必须属于包。拒绝旧/双清单、绝对路径和穿越、符号链接、可移植路径冲突、缺文件、额外文件及摘要替换。M1 包最多 32 MiB、10,001 个文件，读取有界；没有解压、安装、postinstall、探测或执行入口。

身份计算规则如下：

- `manifest_digest`：类型化清单的 JCS JSON 的 SHA256。
- `package_digest`：将**实际包文件**（包含 `extension.yaml`）的 `{path,sha256,size}` 按 path 排序，对该数组的 JCS JSON 计算 SHA256。文件 SHA256 为小写十六进制；结果使用公共 `sha256:` 前缀。
- `core_release_digest`：`CurrentReleaseDigest` 对当前可执行制品的字节计算 SHA256。部署方须保持正在运行的制品不可变。

内置包包含清单、配置 schema、许可证据和对应编译组件的固定源制品。包内容身份与携带它的核心发布分别绑定；核心依赖或其他模块重编译不会改变未修改包的身份。同 ID/version 不可替换 package 或 manifest 摘要；同一不可变包可以绑定多个核心发布，旧绑定保留供历史追溯。修改包内文件时须更新清单文件摘要并升级包版本。独立安装的外部包即使由官方发布，也不能使用这条自举路径。

## 扩展点与权限

`Points()` 是固定 ID/版本及所有者表，返回副本。M1 可登记项如下：

| 扩展点 | 所有者 | 输入 → 输出 |
|---|---|---|
| `asset.validator` v1 | ledger / Profile | `lantai.check-input/v1` → `lantai.check-result/v1` |
| `artifact.processor` v1 | jobs | `lantai.processor-input/v1` → `lantai.processor-result/v1` |

其他公共扩展点仅列 ID 和所有者。M1 不为它们伪造可运行协议。贡献 ID 必须属于包 ID 命名空间且唯一，target 必须有声明；输入/输出 schema 必须与核心扩展点完全一致。M1 只接受 server/node 的 `builtin` runtime/lifecycle；声明能力不等于授权。内置登记不能索取 API、网络或密钥能力，文件权限也不会授予对宿主路径的访问。

`HostTool` 是管理员固定的工具 ID、版本、绝对路径及摘要。`CheckM1` 只检查引用的登记元数据，不读取 PATH/CWD、不执行工具，也不声称工具可运行。M1 自带包不依赖工具；后续真实执行仍须按相应阶段做能力及授权检查。清单里的资源限额是声明，不能据此宣称内置 Go 调用具备进程/内存沙箱。

## 静态登记与恢复

应用用 `New(Deps{DB, Gate, ReleaseDigest})` 组装 Registry。`DB` 是 main；实例启动期间以**同一 Gate 的有效独占维护上下文**调用 `RegisterBuiltins`，完成后才开放业务写入。注册列表由编译代码显式固定；不存在接受外部清单、目录扫描或用户提供 `builtin_release` 的登记 API。

main 中的 `extensions_registrations`、`extensions_contributions` 和 `extensions_release_bindings` 均由 extensions 唯一写入。一次启动静态登记在同一 SQLite 事务中保存，任何冲突或故障全部回滚。重试相同清单和发布绑定无副作用；同批重复包 ID/贡献 ID、跨包贡献所有者冲突或同版本替摘要返回 `IDEMPOTENCY_CONFLICT`。它不是用户启用命令，不生成包 Review、HumanGrant、业务 operation 或运行态 activation。

`Lookup(ctx,id,version)` 读取不可变登记并复验清单摘要；`List(ctx)` 只返回当前发布绑定且仍在编译列表中的包，异常绑定失败关闭。跨重启保留登记与历史发布绑定，M1 不建立 runtime 插件表、动态 loader、DI、依赖图或卸载容器。

## 产物与证据来源

公共 `producer_ref` 固定扩展 ID、版本、包摘要、来源和可选贡献 ID。为保持 v1 兼容，历史 `source=builtin_release` 记录的 `core_release_digest` 可以缺省；schema 通过只表示旧记录可读取，不证明它来自可信发布。新结果的接受必须由登记器核验完整贡献 ID 和当前 `core_release_digest`，缺失发布绑定返回 `EXTENSION_ACTIVATION_STALE`，不能自动补值。`source=package` 禁止冒充核心发布绑定。此结构用于不可变 manifest content 的可选 producer、存储证据记录及 Agent 候选，进入相应内容摘要和命令请求摘要。

`Producer(ctx, contributionID)` 从编译登记生成来源引用；`VerifyProducer(ctx, producer)` 在最终接受时复核：当前发布摘要、编译列表中的包及贡献、持久登记和发布绑定均一致。调用方不能仅靠来源字符串或伪造数据库行成为内置组件。来源身份核验不替代当前权限、输入摘要、资源修订、任务 fence、activation 或人审；领域接受者仍负责各自规则。

历史证据字节、package 来源身份及旧发布绑定保持不可变。Go 的可选字段使用 `omitempty`，缺省来源的旧证据读回及重新规范化仍得到相同字节和摘要。新发布不重写旧证据；新结果接受要求当前发布身份。历史证据是否适用由固定 Profile/业务规则判断，不通过重放旧来源获得新接受权。

当前编译包 `org.lantai.corecheck` v0.1.0 只提供 `org.lantai.corecheck.manifest`。`ValidateManifest` 在调用方提供的已获授权固定字节上校验 manifest 身份及内容摘要，输入最多 8 MiB，输出带 producer 的结构检查结果；它不读取素材目录、不写数据库，也不完成 Profile 验收或人审。合法 `fail` 是证据；解析或身份不符不会变成 pass。processor 的输入/输出 schema 在 M1 固定；M2 的文件协议宿主、外部包导入与治理见[扩展包治理](extension-governance.md)。

M2 起 `VerifyProducer` 另接受 `source=package`：只核对已导入包的 ID/版本/包摘要与已声明贡献，不要求核心发布摘要，也不代表该包当前启用、获审定或可用于某项目；新结果的接受仍由 T06/T09 的激活代次与最终门禁决定。processor-input 可带冻结 `config` 与 `mode: probe`，均为可选字段，旧文档保持有效。

## 验证

运行 `GOCACHE=/tmp/lantai-go-cache go test -race ./internal/extensions ./internal/commands ./internal/contract/schema`。契约样例覆盖正常与拒绝形状；纯规则测试覆盖预留字段不可登记。真实 SQLite 测试覆盖维护上下文、幂等、同版本冲突、事务失败、数据库关闭重开、发布切换及伪造来源拒绝。`internal/catalog` / `internal/provenance` 的接受测试另验证来源字段进入不可变内容及最终复验。
