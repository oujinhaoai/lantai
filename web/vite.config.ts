import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Development only. Production files are served by the existing HTTPS gateway.
export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/api/": "http://127.0.0.1:8080" } },
  build: { sourcemap: false },
});
