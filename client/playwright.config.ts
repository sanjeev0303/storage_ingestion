import { defineConfig, devices } from "@playwright/test";

try {
  process.loadEnvFile(".env.e2e");
} catch {
  // logins can also come from the environment
}

const baseURL = process.env.E2E_BASE_URL ?? "http://localhost:3100";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    // Entrance animations would make colour checks depend on timing.
    reducedMotion: "reduce",
  },
  projects: [
    // Uses the Chrome or Edge already on the machine so no browser download is needed.
    // Set E2E_BROWSER=chromium after `npx playwright install chromium` to use the bundled one.
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], channel: process.env.E2E_BROWSER === "chromium" ? undefined : (process.env.E2E_CHANNEL ?? "chrome") },
    },
  ],
  // The Go API, its workers and the local services must already be running (see README.md).
  webServer: {
    command: "npm run dev",
    url: baseURL,
    reuseExistingServer: true,
    timeout: 120_000,
  },
});
