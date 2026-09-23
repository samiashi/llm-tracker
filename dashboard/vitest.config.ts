import { fileURLToPath, URL } from "node:url";

import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  test: {
    include: ["tests/**/*.{test,spec}.{ts,tsx}"],
    // jsdom for the component tests; the pure helpers do not need it, but one
    // environment for the whole suite costs next to nothing.
    environment: "jsdom",
    setupFiles: ["tests/setup.ts"],
  },
});
