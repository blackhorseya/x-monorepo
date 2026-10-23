import { defineConfig, devices } from "@playwright/test";

// Console smoke against the real server: a ./bin/rpc with no database (in-memory
// repositories, admin RPCs answer admin.no_database) and the production build
// served by vite preview, which proxies /api to it like the dev server does.
// `task web-e2e` builds both first; CI runs it in the console-e2e job.
const rpcPort = Number(process.env.E2E_RPC_PORT ?? 18091);
const webPort = Number(process.env.E2E_WEB_PORT ?? 4173);
const rpc = `http://127.0.0.1:${rpcPort}`;

export default defineConfig({
  testDir: "e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: `http://localhost:${webPort}`,
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      // APP_CONFIG=/dev/null: defaults only, so postgres.url stays empty (viper
      // ignores empty env, so the example config could not be blanked that way).
      command: "../../../bin/rpc",
      url: `${rpc}/api/readyz`,
      env: { APP_CONFIG: "/dev/null", APP_HTTP_ADDR: `:${rpcPort}`, APP_HTTP_DOCS: "false", APP_LOG_LEVEL: "warn" },
      reuseExistingServer: !process.env.CI,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `pnpm exec vite preview --port ${webPort} --strictPort`,
      url: `http://localhost:${webPort}/`,
      env: { CONSOLE_API: rpc },
      reuseExistingServer: !process.env.CI,
    },
  ],
});
