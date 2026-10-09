# Linux M2 云端补验证据归档（2026-10-08）

两份指定原ZIP已在消费端落地并按原输入SHA256验证，包内127/204项清单全部匹配；原报告、日志、矩阵与必要驱动已读取并选择性脱敏归档。完整原件、数据库/二进制/源码压缩包留源码仓库外；逐件哈希及脱敏规则见各archive-manifest.json。初始传输403保留为历史，当前没有文件传输阻塞。

- [M2-10：熔断owner窄范围pass](m2-10.md) / [原报告副本](m2-10/REPORT.md)
- [M2-11：partial，MCP第四上传来源失败](m2-11.md) / [原报告副本](m2-11/REPORT.md)
- [输入校验、范围与选择规则](evidence-manifest.json)

固定 `bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7`，tree `055c669b56034e09315dc3dabfdade04ff4333ba`。本机仅做证据消费与静态核验，未重跑产品测试。实际进度/RUN/缺陷/评审在维护者知识库；不签整卡或GATE。新候选交付后另记录HEAD/tree/driver hash并重跑相应范围，M2-11还需补齐第四上传来源。

## 有界重放准备

只在获准Linux环境使用新的隔离worktree及仓库外新输出目录，不覆盖旧attempt。Go测试驱动在本目录保存为.go.txt，避免go test ./...自动发现；复制到新的隔离checkout后才恢复.go文件名。不编辑公共fixture或共享helper。

M2-10在Go/Python已配置的Linux环境：

```bash
python3 "$archive_dir/m2-10/driver/replay.py" --source "$isolated_repo" --output "$new_evidence"
```

相邻Go驱动读取仅作.go.txt文件名适配；仍复制为internal/extensions/linux_m2_10_owner_test.go，原9阶段/SIGKILL/断言不改。使用--expected-head显式记录新候选。

M2-11先在新隔离checkout准备两个独立测试文件：

```bash
cp "$archive_dir/m2-11/cloud_m211_linux_test.go.txt" "$isolated_repo/tests/integration/cloud_m211_linux_test.go"
cp "$archive_dir/m2-11/cloud_m211_mcp_schema_repro_test.go.txt" "$isolated_repo/tests/integration/cloud_m211_mcp_schema_repro_test.go"
bash "$archive_dir/m2-11/replay.sh" "$isolated_repo" "$new_evidence" both
```

旧基线both预期退出1（chain通过、repro失败），chain预期0，repro预期1。源码改变时先在隔离副本明确调整并记录replay.sh基线保护，不把旧基线pass自动用于新代码。没有完整CI/长测自动启动。
