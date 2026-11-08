import { defineConfig } from "vite";
import base from "./vite.config";

if (process.env.LANTAI_PREVIEW_MOCKS !== "1")
  throw new Error(
    "This isolated preview requires explicit synthetic responses.",
  );
// Never inherit the local development API proxy in the mock-only entry.
export default defineConfig({
  ...base,
  server: { ...base.server, proxy: {} },
  preview: { proxy: {} },
});
