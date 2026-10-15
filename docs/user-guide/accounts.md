# 账户与项目权限

目标：让人和 Agent 用各自身份访问正确的项目。rc.4 仍没有账户管理网页；下面在**自己的可信电脑终端**运行 Lantai CLI。首次管理员仍从[浏览器设置页](setup.md)建立。

## 前提：身份、会话、项目角色缺一不可

| 身份或权限 | 用途 |
| --- | --- |
| 人类管理员 | 密码 + 当前 TOTP 登录，登记身份、分配角色、核对敏感操作 |
| Agent | 管理员登记，例如合成名 `codex@workstation`；用长期凭据换短期会话 |
| 项目角色 | 决定在某项目可读、可写或管理；系统 admin 不自动拥有每个项目 |
| 会话 scope | 决定本次会话能使用哪些能力；有角色仍可能缺 scope |
| HumanGrant | 人本人审核特定动作、目标和修订后，用新动态码取得的短时授权；登录不等于批准 |

`viewer` 用于只读；`contributor` 可提交自己的资源；`owner` 管理项目。其余角色及完整规则见[权限登记表](../contracts/identity-actions.md)。不要给 Agent 人的 admin 会话、密码、TOTP 或恢复码。

新增备份管理候选的网页提供显式“管理员登录（含备份管理权限）”。只在需要管理时勾选；默认浏览会话仍只申请 `self/read`。备份页面同时复核系统管理员角色和 `admin` scope，项目 owner 不等于整馆管理员。详见[备份管理](backup.md)。

## 1. 管理员登录并确认身份

从发行包取得本机对应的 `lantai`，按[版本核对](troubleshooting.md)检查。创建私有工作目录；macOS/Linux 可先执行 `umask 077`，用编辑器创建 `credentials.json`，字段如下。占位文字必须替换，`code` 保持字符串以保留前导零。

```json
{"name":"ada","password":"替换为自己的密码","code":"当前六位码"}
```

```sh
lantai login --server https://library.example:32444 --credentials-file credentials.json --session-file admin-session.json
lantai whoami --server https://library.example:32444 --session-file admin-session.json
```

成功标志：whoami 返回自己的主体 ID 和期望身份。私有文件 Unix 权限应为 0600；Windows 使用仅本人可访问的 ACL。凭据输入文件用后移到受保护存储或删除，长期保留的是密码管理器记录。CLI 不接受命令行明文密码/验证码。若认证失败，查[错误处理](troubleshooting.md)；刚用于登录的动态码不可立即再用于授权。

## 2. 建立项目并给人或 Agent 分配角色

需要管理员会话的 `admin` scope。在私有目录保存项目输入 `project.json`：

```json
{"key":"sample-library","name":"示例资料库","project_type":"production","summary":"合成示例项目"}
```

```sh
lantai project create --server https://library.example:32444 --session-file admin-session.json --input project.json --idempotency-key create-sample-library-001
lantai identity members --server https://library.example:32444 --session-file admin-session.json --project PROJECT_ID
```

把 `PROJECT_ID` 换成创建回执的 `project_id`，保存 members 返回的当前 `revision`。项目刚创建后列表看不到它可能是尚未分配成员角色。为管理员本人或已登记 Agent 分配角色，保存 `command.json`：

```json
{"action":"identity.grant_project_role","command":{"project_id":"PROJECT_ID","expected_revision":1,"principal_id":"PRINCIPAL_ID","role":"viewer","grant":true}}
```

将 ID 和 `expected_revision` 换成实际读到的值；按下一节的三个命令执行。要入藏的 Agent 可按工作需要用 `contributor`，同时需 `read`、`ingest`，完成著录还需 `organize`。不要把示例修订号 1 当成固定值。

## 3. 每个敏感动作都走三步

```sh
lantai identity challenge --server https://library.example:32444 --session-file admin-session.json --input command.json
lantai identity verify --server https://library.example:32444 --session-file admin-session.json --id CHALLENGE_ID --credentials-file code.json
lantai identity execute --server https://library.example:32444 --session-file admin-session.json --input granted-command.json --idempotency-key approve-example-001
```

1. `challenge` 后**本人核对服务端摘要**、目标和修订，保存 challenge ID、operation ID。不要只看 Agent 的解释。
2. 用编辑器把新动态码放入私有 `code.json`：`{"code":"当前六位码"}`。同一个成功时间步不能重用，需等待验证器换码。`verify` 返回 `grant_id`。
3. `granted-command.json` 保留 `command.json` 完全相同的 `action`、`command`，仅在顶层加入返回的 `grant_id`。执行时使用本次固定的幂等键。

成功标志：取得该动作回执，再重新读取 members/principal 验证结果。挑战和授权各有 5 分钟有效期；修订冲突时重读当前状态并重新审核，不把旧 grant 套在改过的命令上。网络中断则先查 `lantai operation --id OPERATION_ID`（加同一 server/session 参数），不自动换键重做。

## 4. 登记 Agent 并签发凭据

按上节流程把 action 改为 `identity.register_principal`，command 为 `{"kind":"agent","name":"codex@workstation"}`；执行命令还要加 `--secret-file registration.json`，用一个未存在的私有输出文件。

然后用 `identity principal --id PRINCIPAL_ID` 读取返回主体的当前 revision；按同样流程执行 `identity.issue_credential`，command 例如 `{"principal_id":"PRINCIPAL_ID","expected_revision":1,"scopes":["read","self"]}`。将修订替换为刚读到的值；执行时加 `--secret-file issued.json`。这里仅授只读 scope，还需按第 2 步单独给项目角色。

`issued.json` 的一次性 `secret` 字段是长期凭据。由本人在私有编辑器中将**该字段的原始值**保存成 `agent-token.txt`，不是复制整个 JSON；保持私有权限。然后：

```sh
lantai session exchange --server https://library.example:32444 --token-file agent-token.txt --session-file agent-session.json
lantai whoami --server https://library.example:32444 --session-file agent-session.json
```

成功标志：whoami 是期望的 Agent，项目列表仅有分配给它的项目。长期凭据默认 180 天，会话最多 12 小时且可能更短；会话过期后本人重新 exchange。不要自动扩大权限或把长期凭据直接当 Bearer 会话。

一次性秘密只交付一次。若执行成功却丢失输出，先根据 operation 核对，再吊销并重新签发；不能假定重放能取回秘密。注销当前会话用 `lantai session end`，同样带 server 和 session-file；它不会删除项目或文件。

## 验证器或恢复码丢失

恢复码用于受限因子恢复，不可填普通登录的动态码栏。恢复码失效、已用或丢失时，不重复猜码。已有其他管理员可走受审核的因子重置；只有一位管理员时，由设备管理员停服务后运行本机 `lantai recover-admin -home DATA_ROOT -admin NAME`，按交互设置新密码和 TOTP。需要原主密钥，成功会使旧凭据失效并生成新恢复码。完整边界见[身份恢复协议](../contracts/identity.md#恢复)，不要为恢复登录清空资料卷。
