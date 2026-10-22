# 离线引用副本与可审阅回退

本模块对应 A18，只处理操作者选定的原生 `.md` 笔记副本，不发网络请求。源目录只读；新副本、回退副本分别写入独立私有目录。它不修改原知识库，不移动素材，不更改模型、工具或中间数据路径，不撤销、回滚或删除任何 Lantai 服务端版本。

## 本机输入与范围

`PrepareReferencePlan(ctx, input, sourceRoot)` 使用版本化本机输入；CLI 接线由主入口负责。

```json
{
  "schema": "lantai.reference-input/v1",
  "notes": ["notes/scene.md"],
  "mappings": [{
    "old_target": "media/hero.png",
    "fixed_ref": "lantai://01ARZ3NDEKTSV4RRFFQ69G5FAV/assets/01ARZ3NDEKTSV4RRFFQ69G5FAW/versions/01ARZ3NDEKTSV4RRFFQ69G5FAX"
  }],
  "preserve": [{"target": "models/example.safetensors", "kind": "model", "reason": "公开模型路径保持原样"}]
}
```

`old_target` 以源语法中的目标字节精确匹配；不解析相对路径、不 URL 解码、不按 basename、后缀、通配或大小写推断，不寻找 latest，不猜版本。`fixed_ref` 必须通过现有 `ids.ParseURI` 的规范固定版本校验：完整 instance/asset/version，不含查询、片段或短时令牌。工具离线校验 URI 结构，不声称已经证明该版本存在或操作者有读取权限；存储验收另由固定版本回拉完成。

映射不得重复或含歧义。`preserve` 可声明 `note/model/tool/intermediate/external/other` 和理由，不能与同一目标的映射冲突。保守拒绝已知模型权重后缀，以及 `models/checkpoints/loras/controlnet/tools/tool/cache/intermediate/tmp/temp/venv/.venv/node_modules/site-packages` 路径段的改链映射；不扫描或改写工程、模型清单、工具命令或中间文件。

这是逐批切换工具：**Complete 只覆盖显式 mappings 在选定笔记中的改链范围**。未映射的正常 wiki/Markdown 目标原样保留并列入 `skipped/not_selected`；显式保护目标列为 `preserved_<kind>`。不要求先分类整篇笔记的所有其他关系，也不声称全馆引用已经迁移。

## 支持与明确拒绝的语法

- 支持单行普通 Markdown 链接及图片：`[label](old)`、`![label](<old path>)`，含明确的单/双引号标题；保留 label、标题和原始换行，仅替换精确目标。
- 支持普通 `[[old]]`、`[[old|alias]]`、`![[old|alias]]`。转换为 `[alias](fixed_ref)` / `![alias](fixed_ref)`；无 alias 时使用旧目标作为 label。不能把外部 `lantai://` URI 塞入 wikilink 并假称它仍是内部笔记名。别名文字保留；不承诺 Lantai URI 在 Obsidian 中自动渲染远程图片、宽度或播放器。
- frontmatter、代码围栏、缩进代码、行内代码、HTML 注释、Obsidian `%%` 注释，以及转义的 `\[[...]]`、`\[label](...)` 都保持原字节。围栏只识别行首至多三个空格后的至少三个 backtick/tilde；关闭围栏后只能有空白，backtick info 不能包含 backtick。正文中的行内 backtick 按行内代码处理，不会把同一行后面的真实链接误吞成整段围栏。保护区域写入 skipped；映射仅出现在保护区而无可改出现时，仍会有 `mapping_has_no_supported_occurrence`。
- 引用式链接定义、HTML 链接、autolink、跨行或不明确的目标、额外 alias 分隔符、复杂转义/表格 wikilink，以及标签内嵌图片/链接等不猜测处理。例如 `[![thumb](old)](https://example.test/full)` 中的映射目标会标为 `unsupported_nested_markdown_label`，即使同文另一处普通链接已经改写，也不能报告 complete。未命中本批映射的嵌套标签保持原样，不阻断无关小批。显式映射碰到不支持或歧义语法时列入 unresolved；没有找到任何可安全修改的该映射也列入 unresolved。不能用“已改一处”掩盖同一显式目标的未解决语法。

这是有界的保守扫描器，不是通用 Markdown 重写器。跨行标签、紧随的 destination 和多行 HTML 会在完整的有界结构范围内检查映射目标，不能只检查首行；无法安全确定闭合范围时，保守检查剩余笔记并拒绝其中选中映射，而不推断完整迁移。整个未支持范围只消费一次，长坏输入不能对每个字符反复扫描剩余全文。额外目标比较受线性大小工作预算约束，预算耗尽标为 unresolved，不能静默截断后报 complete。

## 计划、hash 与 diff

`ReferencePlan` 保存原输入、源根、每篇笔记的完整 `before/after` UTF-8 字节、前后 SHA-256、精确 byte offset edits、可逆 byte diff、unresolved/skipped 和计划摘要。diff 用 `@@ bytes start:end @@` 供审阅，**不是**可直接交给 `git apply` 的统一补丁；回退使用已核验的 before 快照。

计划只读；必须先审阅，再显式调用 `CopyReferenceNotes(ctx, plan, sourceRoot, destination)`。有 unresolved 的计划可输出审阅，但拒绝 copy。读取和复制前复算源文件，任何选中文件变化、计划/映射变化都拒绝沿用原计划。源与目标以 Unicode/case 身份判断重叠；逐路径 NFC、大小写拼写和目录冲突检查与既有工作集一致，任何源/目标链接和源/目标互相包含均拒绝。

硬限：1,000 篇笔记、每篇 before 和 after 各 1 MiB、合计源字节 2 MiB、全批 10,000 映射、全批 10,000 编辑，以及 8 MiB 本机 JSON。单篇扫描也会在编辑上限处安全停止并报告。超限缩小显式批次，不自动截断。计划含完整笔记文本，属于本机私有交付，不应提交到公开仓库或放回被扫描源根。

摘要不充当签名。copy 会读取真实源并重建计划；rollback 会从 `Input + Before` 用同一纯规划器重建 Edits/After/Diff/Skipped/Unresolved，拒绝仅修改 after/hash 后重新计算摘要的伪造计划，并核对原副本绑定。不会只因为攻击者给出一个自洽摘要就相信回退内容。

## 新副本与安全续跑

副本目标必须是新/空的私有根，或已有匹配计划、模式、来源及清单的本工具副本。Unix 新目录为 0700、文件为 0600；Windows 复用现有当前用户 owner/ACL 私有目录创建。已有宽权限目录只拒绝，不自动改权限。

私有 `.lantai-reference-state.json` 绑定计划和模式，独立文件锁防止同副本并发写。所有写通过固定 `os.Root`，在文件/目录和根身份检查后执行；不覆盖已有终稿。逐文件流式写入独立 `.lantai-reference-part`，Sync、完整核对、无覆盖 Link 后发布。重跑同计划只接受完全相同的终稿或与预期一致的 partial 前缀。若在 Link 后、Remove 前中断，final 与 part 同时存在，只有二者都完整等于计划字节、身份复核不变时才移除冗余 part 并恢复完成；不同内容、被缩短的 part 不删除。已被用户修改的副本、partial、未知内容或其他批次必须保留并明确拒绝。

返回 `ReferenceReceipt`，即使局部失败也保留每项 `copied/present/failed/not_attempted` 和摘要。源在预检、每篇写入前和完成前复验；取消、源并发编辑、目标根置换或写入失败不得报 complete。失败可保留部分副本并沿原计划、原根继续，不能盲换状态或覆盖用户后续编辑。

## 新增版本库存与回退

`RollbackReferenceNotes(ctx, plan, copyRoot, rollbackDestination, postCutoverRefs)` 明确要求数组（`[]` 合法，缺省不合法），每个值都须是规范固定 Lantai URI。原顺序和重复项完整保留到回退绑定和回执；不去重、不丢弃、不猜测。这只是调用方声明的切换后新成果库存，`inventory_scope=caller_declared_only_not_whole_library`，没有自动查询全馆或保证清单穷尽的含义。

回退先验证整个改链副本仍等于计划 after；有用户续写、缺失或未完成 partial 时拒绝。然后在**第三个新独立私有目录**输出完整 before，保留原目录布局和换行。原知识库、改链副本及所有服务端 v1/v2 均保留。重跑相同回退目标只接受原 plan 和原 post refs 库存；更换清单必须选择新的可审阅回退目录。

例如：合成 v1 固定引用改入笔记副本，随后制作并保存 v2 固定回执；回退输出旧本机链接，同时在回执中完整保留 v2 URI。恢复旧入口不等于删除新增成果；后续找回/继续 v2 应使用其固定版本引用。

## 自动验证边界

```sh
go test -race ./internal/client -run '^TestReference' -count=1
go vet ./internal/client
GOOS=windows GOARCH=amd64 go test -c ./internal/client -o /tmp/lantai-reference-client.test.exe
```

测试全部为临时合成笔记，覆盖精确链接/图片/wiki/embed、label/alias/CRLF、代码/注释/转义、未选择/模型/工具/中间路径、未知或歧义语法、同名文件和大小写/NFC冲突、长坏输入、篡改重算计划、原文变化、partial/取消后续跑、副本续写冲突、根目录置换，以及 v1→改链副本→显式 v2 库存→新回退副本。真实 core 的新版本库存验收由集成测试验证，不从离线 URI 结构校验推导版本存在。Windows 交叉编译不能代替 Windows 本机 ACL/文件协议验收。
