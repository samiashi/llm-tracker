import { fileURLToPath, URL } from "node:url";

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build lands directly in the server's embed directory, so `go build`
// picks up whatever the frontend last produced without a copy step.
export default defineConfig({
  plugins: [react()],
  // "@" points at src. Relative imports that climb out of a directory are
  // brittle: moving a file silently changes what "../.." means.
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: { outDir: "../server/internal/web/dist", emptyOutDir: true },
  server: {
    // Loopback only, like the server. The proxy passes on this Host, which
    // the server checks names this machine.
    host: "127.0.0.1",
    port: 5178,
    // In dev the SPA runs on Vite and proxies the API, so the dashboard can
    // hot-reload against the real server instead of a mock.
    proxy: { "/v1": "http://127.0.0.1:8790" },
  },
});
