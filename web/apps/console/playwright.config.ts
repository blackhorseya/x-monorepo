import { defineConfig, devices } from "@playwright/test";

// Console smoke against the real server: a ./bin/rpc and the production build
// served by vite preview, which proxies /api to it like the dev server does.
// `task web-e2e` builds both first; CI runs it in the console-e2e job.
//
// Two modes, picked by E2E_DATABASE_URL:
//   unset → no database: in-memory repositories, admin RPCs answer admin.no_database
//   set   → bin/migrate up, then rpc on that Postgres: poison / prune run for real
// Specs read the same variable (e2e/mode.ts) to assert the right behaviour.
//
// A second pair (project "auth", e2e/auth/) runs the same build against an rpc
// with auth.mode=required and two API keys, no database: what the console does
// when the server wants a credential. Its own preview port = its own origin, so
// the credential it stores in localStorage never leaks into the other project.
const rpcPort = Number(process.env.E2E_RPC_PORT ?? 18091);
const webPort = Number(process.env.E2E_WEB_PORT ?? 4173);
const authRpcPort = rpcPort + 1;
const authWebPort = webPort + 1;
const rpc = `http://127.0.0.1:${rpcPort}`;
const authRpc = `http://127.0.0.1:${authRpcPort}`;
const db = process.env.E2E_DATABASE_URL;
const bin = "../../../bin";
const baseEnv = { APP_CONFIG: "/dev/null", APP_HTTP_DOCS: "false", APP_LOG_LEVEL: "warn" };

// Keys the auth project's server accepts (e2e/auth/keys.ts has the same values).
export const OPS_KEY = "e2e-ops-key-0123456789abcdef";
export const ADMIN_KEY = "e2e-admin-key-0123456789abcdef";

export default defineConfig({
  testDir: "e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      testIgnore: /\/auth\//,
      use: { ...devices["Desktop Chrome"], baseURL: `http://localhost:${webPort}` },
    },
    {
      name: "auth",
      testMatch: /\/auth\//,
      use: { ...devices["Desktop Chrome"], baseURL: `http://localhost:${authWebPort}` },
    },
  ],
  webServer: [
    {
      // APP_CONFIG=/dev/null: defaults only, so postgres.url is exactly what
      // E2E_DATABASE_URL says (viper ignores empty env, so the example config
      // could not be blanked that way). readyz pings Postgres when it is wired.
      command: db ? `${bin}/migrate up && exec ${bin}/rpc` : `${bin}/rpc`,
      url: `${rpc}/api/readyz`,
      env: { ...baseEnv, APP_HTTP_ADDR: `:${rpcPort}`, ...(db ? { APP_POSTGRES_URL: db } : {}) },
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
    {
      // auth.mode=required, in-memory: probes stay open (readyz is the ready signal),
      // every RPC wants X-API-Key; the admin key carries the admin role, ops does not.
      command: `${bin}/rpc`,
      url: `${authRpc}/api/readyz`,
      env: {
        ...baseEnv,
        APP_HTTP_ADDR: `:${authRpcPort}`,
        APP_AUTH_MODE: "required",
        APP_AUTH_API_KEYS: `ops=${OPS_KEY},admin=${ADMIN_KEY}:admin`,
      },
      reuseExistingServer: !process.env.CI,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `pnpm exec vite preview --port ${authWebPort} --strictPort`,
      url: `http://localhost:${authWebPort}/`,
      env: { CONSOLE_API: authRpc },
      reuseExistingServer: !process.env.CI,
    },
  ],
});
