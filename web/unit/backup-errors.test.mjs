import test from "node:test";
import assert from "node:assert/strict";
import { APIError, message } from "../src/api.ts";

test("backup artifact errors distinguish unsafe files from unavailable storage", () => {
  const hardlinks = message(
    new APIError(
      "STORAGE_UNAVAILABLE",
      503,
      undefined,
      "backup_package_hardlinks",
    ),
  );
  assert.match(hardlinks, /硬链接计数异常/);
  assert.match(hardlinks, /不要放宽权限或删除未知链接/);
  assert.match(
    message(
      new APIError(
        "STORAGE_UNAVAILABLE",
        503,
        undefined,
        "backup_package_integrity",
      ),
    ),
    /内容校验未通过/,
  );
  assert.match(
    message(
      new APIError(
        "STORAGE_UNAVAILABLE",
        503,
        undefined,
        "backup_package_identity",
      ),
    ),
    /标识、大小或摘要不一致/,
  );
  assert.match(
    message(
      new APIError("STORAGE_UNAVAILABLE", 503, undefined, "backup_unavailable"),
    ),
    /备份存储暂不可用/,
  );
});
