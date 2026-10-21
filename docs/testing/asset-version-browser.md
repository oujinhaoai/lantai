# 固定版本网页浏览验证

`AssetVersionView` 复用同源精确版本读取和既有 `FilePreview`，不新增存储、权限或 ReadGrant 协议。应用外壳继续拥有登录与路由。

## 集成接口

```tsx
import { AssetVersionView } from "./asset-version-view";

<AssetVersionView assetID={pathname.slice(8)} route={route} expire={expire} />
```

`main.tsx` 已通过这一调用挂载组件，并移除旧 `AssetDetail`。应用外壳继续负责根路由和认证，API 模型沿用原契约。

- `/#/assets/<asset-id>?version=<version-id>` 始终读取指定版本。未提供 `version` 时读取最新版本，再按精确版本取得清单。空、重复、非规范版本标识均报错，不退回最新版。
- “固定此版本链接”可用浏览器保存/复制；它只含资产、版本和返回上下文。永久 `lantai://` 引用从精确版本显示。短期读取 URL 只存在当前预览组件内存。
- “按版本标识打开历史版本”支持粘贴已知版本 ID。现有 API 没有历史列表入口，本版不枚举或猜测版本。
- 当前资产著录独立于不可变版本。指定旧版时先核验精确清单；若当前著录因最新版本权利等原因返回 `NOT_FOUND`，仍显示有权读取的旧版，并明确省略当前著录。其他错误正常显示，会话错误交回原 `expire`。
- 精确响应的资产、项目、版本、版本号、清单摘要和馆实例永久引用必须一致。换路由卸载旧请求和预览，短时 URL、选中文件与迟到结果不能跨版本复用。浏览器文件预览/下载仍逐次请求同一精确版本的现有用途授权。

## 自动验证

正常集成后的运行方式与既有网页一致：

```sh
npm ci --prefix web
npm run lint --prefix web
npm run build --prefix web
cd web
npm test -- tests/asset-versions.spec.ts tests/media.spec.ts
```

`asset-versions.spec.ts` 使用合成响应验证前端版本选择、清单绑定、预览/下载请求、当前著录不可读、拒权、显式重试与迟到请求，不把受控响应当成真实服务器授权验收。正常模式使用现有 HTTPS 临时实例完成真实登录；`media.spec.ts` 另用已有真实存储、授权和合成 PNG/WAV/MP4 等文件验证类型预览及降级。

独立开发而尚未接主应用时，在 `web/` 执行：

```sh
node tests/build-asset-version-fixture.mjs
LANTAI_ASSET_VERSION_HARNESS=1 npm test -- tests/asset-versions.spec.ts
```

脚本将 `web/tests/fixtures/asset-version-view.html` 作为额外 Vite HTML input 构建，并复制至既有合成 HTTPS 服务允许的 `/assets/version-fixture.html` 路径。额外 input 仅用于开发测试，默认生产构建不包含此夹具。它提供无应用外壳的真实组件宿主，固定的合成 CSRF 值无实际认证能力，测试 API 均由 Playwright 拦截；不能用它声称登录或后端已通过。无需修改共享夹具、Playwright 配置或包依赖。完成后再执行正常 `npm run build`，即可移除 `dist` 中的夹具入口。

组件测试不替代目标网关、大文件、NAS、真实素材或真实 DCC 工程验收；也不提供在线 3D 查看、转码、编辑或历史著录重放。
