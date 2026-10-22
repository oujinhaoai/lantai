# 素材管理操作

`lantai asset --help` 是现有公共 REST 和本地显式副本的编排入口，不新增存储、身份、审定、发布或备份真源。其 `profile`、`import plan`、`references` 离线运行；其余命令使用已有会话、项目角色和用途授权。所有命令返回 JSON，非零退出时仍可能返回部分结果，必须同时检查退出码与 `complete/state`。

## 著录和查找

先准备一个本机 JSON/YAML 请求，`input` 遵循原 [PushInput](client.md)；`profile` 遵循[画像契约](asset-profile.md)。下面仅是描述性字段示例；原始 rights、项目与文件清单必须由实际授权输入明确提供。

```json
{
  "input": {
    "project_id": "01J00000000000000000000001",
    "slug": "synthetic/hero-a-design",
    "content": {"asset_type": "image", "files": [{"path":"design.svg","role":"source"}]}
  },
  "profile": {
    "characters": ["hero-a"], "source": "synthetic",
    "business_state": "active", "legacy_state": "awaiting-human-review",
    "legacy_version": "v001"
  }
}
```

```sh
lantai asset profile --input profile-request.json --output push-input.json
# 审阅 push-input.json，明确 rights 后，用已有 push --state 入口入藏。
lantai asset find --input find-request.json --server "$LANTAI_SERVER" --session-file session.json
```

查找请求示例为 `{"filter":{"characters":["hero-a"],"business_state":"active","source":"synthetic"},"options":{"page_size":100}}`。多个角色、状态、来源和 tags 都取精确交集；服务端仍逐页做授权过滤。`complete=true,truncated=false` 且零退出才表示遍历完成。空授权页仍继续；游标过期有界重启并丢弃旧结果；撤权、会话失效或协议失败不输出旧累积资产。预算不足返回 partial 和非零退出，应提高显式预算后重跑。检索读取异步投影，刚提交或刚修改的著录可能尚未进入该投影；complete表示已遍历当前投影，不是提交的读取确认。入藏后立即核对时使用固定asset/version回执，等待正常投影收敛后再作标签批次查找。

画像初始投射和冻结快照不能代替当前著录编辑。追加版本只冻结本版 profile，不自动改当前 tags；修改当前标签仍需原 `metadata get/set`、`If-Match` 和稳定幂等键。`active/pending_review` 都不是人审、发布或许可事实。

## 固定版本工作集

准备[工作集输入](workset.md)，显式列出 asset/version、各自目录、入口及必需文件。不同资产有各自独占目录，工程内引用应事先使用恰当相对路径。

```sh
lantai asset workset plan --input workset.json --directory working-copy --server "$LANTAI_SERVER" --session-file session.json --output workset-plan.json
lantai asset workset pull --input workset.json --directory working-copy --server "$LANTAI_SERVER" --session-file session.json --purpose archive_review --output workset-receipt.json
```

plan 不创建工作目录；pull 重查清单和目标后才下载。相同内容复验，已有不同内容拒绝覆盖；相同输入/目录可续传。固定版本不因新版本发布改变。工作集仅核对声明的依赖，不自动解析、改写或执行 Blender/ComfyUI/UE 工程。原有 `.lantai-*` 锁/断点文件属于客户端状态，不是素材原件。

## 显式批次入藏与验证

输入 schema 为 `lantai.asset-import-input/v1`，`assets` 每项含 `key`、相对于源根的 `directory`、`classification`（final/intermediate/unknown）及原 `push`。只上传显式 `push.content.files`；`excluded` 用安全相对路径和原因记录排除，其他发现项列入 plan 的 `uncovered`，不会自动选入。分组、文件角色、来源和 rights 由操作者判断，不按扩展名或历史版本字符串猜测身份。

```sh
lantai asset import plan --input import-input.json --directory source --output import-plan.json
# 审阅完整 plan、选中文件哈希、字节、分类、rights 与 uncovered 后才执行：
lantai asset import apply --input import-plan.json --directory source --state import-state --output import-report.json --server "$LANTAI_SERVER" --session-file session.json
lantai asset import verify --input import-plan.json --report import-report.json --directory source --state import-state --destination verified-copy --output verify-report.json --server "$LANTAI_SERVER" --session-file session.json
```

源、state、验证目标必须独立且不嵌套；目录的NFC/Unicode大小写等价拼写也视为同名，避免不区分大小写的磁盘借别名写回源；plan/report输出须在这些目录及工作副本之外，不覆盖输入、原Push状态或素材文件。状态目录、报告、画像与计划可能带来源路径，按私有资料保管，不提交公开仓库。会话凭据只走原私有文件或环境变量，结果不保存读取授权 URL。

指定 `--output` 时，在长操作开始前固定输出父目录句柄；结果以相对该句柄的私有临时文件和原子替换发布。父目录或其祖先在期间被置换、或原有路径别名被改指向时，保存失败并非零退出，不向新目标重解析写入；回退还会保护计划内的原笔记根。已有输出必须是私有普通文件，不能借输出覆盖symlink目标。Unix临时文件0600；Windows以相对目录句柄创建并沿用当前用户owner/私有ACL策略。结果保存失败时保留原操作state及已完成副本，不能将其当作服务端未提交。

plan 流式散列选中文件并冻结完整请求；apply 开始及每项续办前复核源，变化就拒绝。每项沿用原 Push 状态与创建/提交幂等键；未知结果或丢失原状态不能换键盲重试。已 committed 的回执可重复读取；`description_pending` 表示字节已提交、著录待完成，批次仍非零退出。同 plan/state 续办原提交，取得合法 organize 授权后补著录，不新建版本。后续 verify 可证明已提交字节，不把待著录批次改称管理完成。

verify 读取固定回执和清单，在空的独立目标逐文件核对完整路径、大小、SHA-256，并允许同绑定目录续办。它不证明第三方软件能打开文件或已满足全部制作依赖。任何阶段均不移动、删除源件；失败保留 source/state/已验证副本，修复问题后按原计划续办。源件是否最终退役属于另一次经授权的迁移决策，至少等独立恢复和实际工作流验收后再讨论。

## 笔记副本改链与回退

先准备[引用副本输入](reference-copies.md)，只列选中的相对 Markdown 笔记路径，以及旧链接目标到规范 `lantai://<instance>/assets/<asset>/versions/<version>` 的显式映射。固定引用来自已核对的提交/调阅回执；离线规划器只验证语法，不连接服务端证明版本存在或有权使用。普通笔记、工具、模型和其他未选择目标保持原样，列为 skipped；未解决的显式映射使 plan 非零退出并阻止 copy。`complete` 只表示选中笔记的显式映射范围，不表示全库改链或所有依赖已盘点。

```sh
lantai asset references plan --input references-input.json --directory selected-notes --output references-plan.json
# 审阅完整 before/after、哈希和逐处 diff 后创建新的独立私有目录：
lantai asset references copy --input references-plan.json --directory selected-notes --destination note-copy --output reference-copy-receipt.json
# post-cutover-refs.json 是显式固定版本URI数组；没有新增版本也须写 []：
lantai asset references rollback --input references-plan.json --directory note-copy --destination rollback-copy --post-cutover-refs post-cutover-refs.json --output reference-rollback-receipt.json
```

copy 复核源和计划，重复执行核对既有副本，不覆盖本地编辑；rollback 复核已改副本，将原文另存到新的独立目录，并原样保留调用方声明的切换后版本清单。回退不重写源或已改副本，也不回退/删除服务端版本。新增版本数组不是自动全馆清点、备份或保留策略；实际切换前必须通过正常回执维护完整清单并另验恢复。语法不支持的引用用显式未解决项报告，不做全文路径替换。计划包含选中笔记的完整文本，应私有保存。真实知识库切换和源件退役需另行授权。

## 验证范围

通用程序只保存原件与版本，不在服务端转码或自动运行工程。[合成夹具](../../tests/fixtures/asset-management/README.md)生成九类资料，独立记录字节回拉、格式读取和原生继续编辑的证据；`go test -run 'TestAssetManagement|TestAssetReferences' ./tests/integration` 可组合验证真实核心、客户端与合成笔记副本。生成语料通过 `LANTAI_ASSET_FIXTURE_ROOT` 显式提供，未提供时该专项 corpus 测试明确跳过，不能记为九类通过。浏览器验证见[固定版本浏览](../testing/asset-version-browser.md)。目标 NAS、大文件性能、容量和整馆灾备仍须独立验收。


## 恢复后的客户端核对

本地 `apply` 的 `complete` 记录历史回执，`current_remote_verified=false`；恢复到较早捕获点时旧回执不能证明版本仍存在。迁移验收必须执行固定版本的 `verify`，只有本次真实远端及文件校验全通过才得到 `current_remote_verified=true`。保全原状态，不改 origin/摘要/键；原操作不存在或无权时停止并保留证据，不能盲换状态重新入藏。详见[批次导入的恢复契约](asset-import.md#灾后恢复与客户端回执)。
