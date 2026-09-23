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
    // The same host as the server, so the session cookie from signing in on
    // :8790 is sent here too: cookies are per host, not per port.
    host: "127.0.0.1",
    port: 5178,
    // In dev the SPA runs on Vite and proxies the API, so the dashboard can
    // hot-reload against the real server instead of a mock.
    proxy: { "/v1": "http://127.0.0.1:8790", "/auth": "http://127.0.0.1:8790" },
  },
});
