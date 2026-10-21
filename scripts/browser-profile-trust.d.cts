import type { BrowserContext } from "../web/node_modules/@playwright/test";
export function trustLocalTestCA(
  context: BrowserContext,
  caFile: string,
): Promise<void>;
