// Explicit opt-in build for the isolated component tests. Default production
// builds never include this fixture. Keep all generated files inside dist/.
import { build } from "vite";
import { copyFile } from "node:fs/promises";
import { fileURLToPath, URL } from "node:url";
import path from "node:path";

const root = fileURLToPath(new URL("../", import.meta.url));
await build({
  root,
  configFile: path.join(root, "vite.config.ts"),
  build: {
    rollupOptions: {
      input: {
        app: path.join(root, "index.html"),
        assetVersionFixture: path.join(
          root,
          "tests/fixtures/asset-version-view.html",
        ),
      },
    },
  },
});
// The existing HTTPS fixture permits only / and /assets/ static routes.
await copyFile(
  path.join(root, "dist/tests/fixtures/asset-version-view.html"),
  path.join(root, "dist/assets/version-fixture.html"),
);
