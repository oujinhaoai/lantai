# 真实工程编辑与新版本回交验收

`TestAssetNativeEditRecommitAndReopen` 在同一个运行中的真实核心内完成：合成BLEND入藏v1→固定工作集回拉→实际Blender打开相对贴图/链接库、CPU渲染、位移编辑并另存→同一asset/base_version提交v2→固定回拉v2、逐文件比对、Blender再次重开渲染→重新拉回v1验证原件不变。它使用现有客户端和公共API，不启动第二套存储或备份。

先按[夹具说明](../../tests/fixtures/asset-management/README.md)在新目录生成并验证完整合成语料，然后显式指定语料及已安装Blender：

```sh
LANTAI_ACCEPT_NATIVE_RECOMMIT=1 \
LANTAI_ASSET_FIXTURE_ROOT=/path/to/generated-synthetic-corpus \
LANTAI_BLENDER=/path/to/blender \
go test -race -v -timeout=10m ./tests/integration -run '^TestAssetNativeEditRecommitAndReopen$' -count=1
```

未显式启用时普通CI明确跳过；不会自动寻找用户工程、下载安装软件或更改已有软件设置。临时实例、工作副本、Python驱动和恢复状态均为测试新建。Blender以factory-startup和disable-autoexec运行，脚本只打开本次合成核心回拉的场景。

本机已验证Blender5.2.1 LTS。测试不证明任意Blender版本、任意工程依赖、目标NAS耐久性或整馆恢复通过。它是对“字节回拉”和“异目录原生续做”之上的新版本回交证明，不允许以复制旧v2替代实际编辑。ComfyUI的无模型真实复跑另见夹具验证器，未纳入这个Blender专项。


## 完整包恢复后的原生续做

`TestAssetRecoveryBlenderEditRecommitAndReopen` 在相同的真实 Blender 流程前插入完整包恢复：源 core 通过协调器备份并关闭，源数据目录移开；空目标只用完整包恢复，重建管理员因素、执行 Recover/深度 FSCK、接受恢复点，再为原合成主体合法签发新会话。工作副本来自恢复目标的固定版本下载。随后实际修改场景、提交同资产新版本、重开渲染，并复核原版哈希。

`TestAssetRecoveryComfyEditRecommitAndReopen` 对无模型 ComfyUI 配方执行同一恢复路径。驱动修改恢复回拉的 UI/API 图，将缩放从 64×64 改为 128×96，调用已安装版本的正式校验器、执行器和 LoadImage/ImageScale/SaveImage 节点；将修改后的图、实际运行记录和 PNG 入藏为原资产 v2。再次固定回拉并由 ComfyUI 重执行，核对像素、尺寸及嵌入的工作流，最后确认 v1 所有文件不变和重复提交未产生 v3。

```sh
LANTAI_ACCEPT_NATIVE_RECOMMIT=1 \
LANTAI_ACCEPT_COMFY_RECOVERY=1 \
LANTAI_ASSET_FIXTURE_ROOT=/path/to/synthetic-corpus \
LANTAI_BLENDER=/path/to/installed/blender \
LANTAI_COMFY_PYTHON=/path/to/installed/comfy/python \
LANTAI_COMFY_SOURCE=/path/to/installed/ComfyUI \
go test -race -v -timeout=10m ./tests/integration \
  -run '^TestAssetRecovery(Blender|Comfy)' -count=1
```

可选 `LANTAI_AVAILABILITY_EVIDENCE_DIR` 为已存在的私有空证据根；每项测试建立唯一子目录，保留修改结果、重开图、原生工具日志与验证 JSON。重跑应使用新的证据根，不覆盖旧证据。缺少显式授权开关时测试跳过；显式启用而缺软件或执行失败时失败，不安装或下载补齐。ComfyUI 不启动服务、不加载模型和自定义节点，输入/输出/缓存/数据库仅在新目录；Python 审计是附加检查而非安全沙箱。Blender 使用工厂配置和禁止自动执行，独立用户配置目录。

这证明记录版本的合成工程可在恢复后继续制作，不承诺任意原生插件、模型、GPU、工程版本或第三方节点兼容，也不等于目标设备或独立验收通过。
