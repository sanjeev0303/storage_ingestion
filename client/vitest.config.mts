import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

const src = (path: string) => fileURLToPath(new URL(path, import.meta.url));

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": src("./src"),
      // "server-only" throws outside the Next.js server build; tests import server code directly.
      "server-only": src("./src/test/empty.ts"),
    },
  },
  test: {
    environment: "jsdom",
    // Relative URLs (/api/proxy/...) resolve against this origin, which is where the msw handlers point.
    environmentOptions: { jsdom: { url: "http://localhost:3100" } },
    globals: true,
    testTimeout: 30_000,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    css: false,
  },
});
