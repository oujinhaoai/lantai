import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

// Development only. Production files are served by the existing HTTPS gateway.
export default defineConfig({
  plugins: [react()],
  // A separately developed manual can land without editing the app shell.
  // Never advertise a help page that is missing from this exact build.
  define: {
    "import.meta.env.LANTAI_HELP_AVAILABLE": JSON.stringify(
      existsSync(
        fileURLToPath(new URL("./public/help/index.html", import.meta.url)),
      ),
    ),
  },
  server: { proxy: { "/api/": "http://127.0.0.1:8080" } },
  build: { sourcemap: false },
});
