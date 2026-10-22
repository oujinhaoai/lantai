# M1-07 原生续做证据闭合

`lantai.native-continuation/v1` 是人类对原生打开、编辑、保存及回交的见证，JSON Schema 是 `schemas/ledger/v1/native-continuation.schema.json`。服务器核验固定文件、版本、当前授权与 T03 审定，不声称自动证明 DCC 进程运行。只上传报告、勾选清单或把设备 Job 记为 succeeded 均不能通过。

## 提交与复核

1. 固定本批次工作集记录和原输入版本；在真实原生软件完成工作，保留应用名/版本/平台、开始结束时间、入口、观察及未解决项。支持证据作为正常资产入馆，保留固定版本和完整摘要；不要将口令、私钥或机器目录作为证据。
2. 通过原资产追加版本接口，以固定 base 提交完整回交版本。返回版本必须是该 base 后的下一已提交版本，属于同一资产和项目，由见证操作者本人提交；原入口路径仍存在，且至少一个原有同路径文件的实际 SHA/大小改变。单纯改名、删文件、改角色或 metadata 不算编辑。源工作集继续固定旧版。
3. 用现有 T05 生产任务提交一个普通 config 资产，唯一文件 `native-continuation.json`，角色 `record`。内容使用上述 schema，绑定 instance/project/batch/workset/用途/恢复纪元、真实人类 operator、入口、base→result 固定引用和摘要、至少一项支持证据。设 `evidence_basis=human_attested`、`NoAI=true`、`redistribute_raw=false`；报告是作者自己的内部见证文件，选择有依据且允许原有核心检查的权利，不能据此扩大输入或输出的用途。报告不是新的 management-record envelope，也不使用私有档案的保留许可证冒充生产用途许可。
4. 项目先通过正常配置审定准备 profile_id 为 `native-continuation` 的 acceptance-profile：`distinct_actor_rule=maker_checker`、`defaults.publication=manual`，保留现有不可豁免的 integrity/schema/license_evidence/purpose 检查。没有预批准 profile 或跳过 corecheck 的入口；项目首个 profile 仍走原有显式初始化审定。
5. 通过原 T06 `org.lantai.corecheck.manifest` Job 产生真实四项检查，向 `POST /api/v1/review-targets` 提交报告固定版本、原 T05 round/attempt/fence、profile_ref 和真实 evidence_ids。当前合格 owner/reviewer 人类按现有 `review.allow_self_human` 政策审阅报告及支持文件：允许时操作者可自审，禁止时仍须独立审定。本人会话通过新鲜 TOTP 换取精确绑定的 HumanGrant，再通过原 `POST /api/v1/human/prepare`、`POST /api/v1/identity/challenges/{challenge_id}/verify`、`/api/v1/human/execute` 执行 `ledger.record_review`，批准精确 target/revision/manifest。原接口也支持拒绝、退回、撤销及原操作键重放；不建立新的审批表或新授权动作。
6. 在批次的新不可变修订 `evidence.native_continuations` 中加入报告的固定 `{asset_id,version_id}`。网页已有“按当前策略人审的原生续做报告”输入；保存后仍调用 `POST /api/v1/management/batches/check`，不会因填入引用自动接受。

## 当前接受边界

服务器在共享安全保护下重新读取报告完整字节并验证 schema；复核当前恢复纪元、项目、批次、用途、工作集、入口、操作者及回交版本。每个需要原生续做的工作集必须被覆盖，重复或外来证据拒绝。所有附加文件共用现有全局读取/数量预算。

批准来自 ledger 当前 effective_review_id，不能由 JSON 提供。审核人必须是当前 active 的合格人类 owner/reviewer，自审是否允许由当前 `review.allow_self_human` 判断，HumanGrant 必须来自原 T03 接受记录。服务器再次检查原 profile 当前批准、完整检查的当前生产者与授权适用性，以及原 T05 attempt/round/output 和所属 Flow：取消或失败失效，正常完成可保留精确批准。审核、profile 或成员撤销会使后续检查不再通过；恢复后的旧纪元报告不得重新获权。旧记录保留历史，不自动补发或换键。

报告 outcome 非 completed 或 unresolved 非空仍为 unknown。只有完整证据与遵循当前 T03 政策的批准全部成立时 `native_continuation=passed`，code 为 `current_human_attestation`。这表示人审证据满足该门禁，并不是 DCC 机器证明。整体 `ready` 还要求恢复、rights/用途、文件、工作集、改链、责任声明等其他门禁均 passed；本机制不清除恢复后未知或 Job 未知副作用。

## 防伪能力与限制

防止客户端伪造“approved/complete”、外来/过期版本、错工作区、错摘要、违反当前政策的自审、无 HumanGrant、无真实核心检查、被撤销批准、错误回交和省略入口。文件摘要证明保存了哪些字节；它不能单独证明字节由指定 DCC 产生。人类可能作错误或虚假见证，两个串通的人或伪造的外部日志无法由普通服务端读文件彻底识别。需要更强机器可信度时须另设经授权、可验证来源的 DCC 运行证明，不得将当前协议重新标为 machine_verified。

合成测试用 `SyntheticProtocolFixture` 并在观察文件明确“未运行 DCC”，只验证协议闭合、当前权限和失效行为；它不产生真实原生软件验收通过声明。

人类自审由原 T03 服务端依据固定 target 的 maker 与实际审定 actor 推导，并在不可变回执中记录 `SelfReview`；不能由客户端声明。新批准继续校验本人交互会话、精确动作/目标/请求及有效 HumanGrant；政策收紧后，原自审记录仍保留，但不能支持不符合当前政策的新接受。历史批准的只读适用性复验不要求旧 grant 尚未过期；已经提交的原 operation 仍按原回执幂等重放，不重复批准或消耗新因素。
