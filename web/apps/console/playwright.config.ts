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
// with auth.mode=required, two API keys and an HS256 JWT secret, no database:
// what the console does when the server wants a credential, for both kinds. Its
// own preview port = its own origin, so the credential it stores in localStorage
// never leaks into the other project.
// E2E_CONSOLE_URL: run the "chromium" project against a console served from somewhere else —
// e2e/image.sh builds web/apps/console/Dockerfile and points this at the container, so the
// nginx config (SPA fallback, /api proxy, cache headers) is what gets tested, not vite preview.
// The rpc is still started here; the container reaches it through the host. (image.sh runs
// --project chromium only; the auth pair still starts but is unused in that mode.)
const consoleURL = process.env.E2E_CONSOLE_URL;
const rpcPort = Number(process.env.E2E_RPC_PORT ?? 18091);
const webPort = Number(process.env.E2E_WEB_PORT ?? 4173);
const authRpcPort = rpcPort + 1;
const authWebPort = webPort + 1;
const roleRpcPort = rpcPort + 2;
const roleWebPort = webPort + 2;
const rpc = `http://127.0.0.1:${rpcPort}`;
const authRpc = `http://127.0.0.1:${authRpcPort}`;
const roleRpc = `http://127.0.0.1:${roleRpcPort}`;
const db = process.env.E2E_DATABASE_URL;
const bin = "../../../bin";
const baseEnv = {
  APP_CONFIG: "/dev/null",
  APP_HTTP_DOCS: "false",
  APP_LOG_LEVEL: "warn",
};

// What the auth project's server accepts: two API keys (ops has no role, admin
// has the admin role) and HS256 tokens signed with JWT_SECRET for JWT_ISSUER /
// JWT_AUDIENCE (e2e/auth/jwt.ts mints them; the server requires exp and pins
// iss / aud, so a token for another issuer is auth.invalid_credential).
export const OPS_KEY = "e2e-ops-key-0123456789abcdef";
export const ADMIN_KEY = "e2e-admin-key-0123456789abcdef";
// The "auth-role" pair: same shape, but auth.admin_role is "operator" (a supported setting), so
// the key holding "operator" is the admin and the one holding "admin" is not.
export const OPERATOR_KEY = "e2e-operator-key-0123456789abcdef";
export const NOT_OPERATOR_KEY = "e2e-notoperator-key-0123456789abcdef";
export const JWT_SECRET = "e2e-hs256-secret-0123456789abcdef0123456789abcdef"; // ≥ 32 bytes
export const JWT_ISSUER = "https://e2e.example.com";
export const JWT_AUDIENCE = "x-monorepo-console-e2e";

export default defineConfig({
  testDir: "e2e",
  // DB mode also runs one bin/worker (e2e/worker.setup.ts) so reservations happen.
  globalSetup: "./e2e/worker.setup.ts",
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
      testIgnore: /\/auth(-role)?\//,
      use: {
        ...devices["Desktop Chrome"],
        baseURL: consoleURL ?? `http://localhost:${webPort}`,
      },
    },
    {
      name: "auth",
      testMatch: /\/auth\/.*\.spec\.ts$/, // helpers.ts / jwt.ts beside them are not tests
      use: {
        ...devices["Desktop Chrome"],
        baseURL: `http://localhost:${authWebPort}`,
      },
    },
    {
      name: "auth-role",
      testMatch: /\/auth-role\/.*\.spec\.ts$/,
      use: {
        ...devices["Desktop Chrome"],
        baseURL: `http://localhost:${roleWebPort}`,
      },
    },
  ],
  webServer: [
    {
      // APP_CONFIG=/dev/null: defaults only, so postgres.url is exactly what
      // E2E_DATABASE_URL says (viper ignores empty env, so the example config
      // could not be blanked that way). readyz pings Postgres when it is wired.
      command: db ? `${bin}/migrate up && exec ${bin}/rpc` : `${bin}/rpc`,
      url: `${rpc}/api/readyz`,
      env: {
        ...baseEnv,
        APP_HTTP_ADDR: `:${rpcPort}`,
        ...(db ? { APP_POSTGRES_URL: db } : {}),
      },
      reuseExistingServer: !process.env.CI,
      stdout: "ignore",
      stderr: "pipe",
    },
    ...(consoleURL
      ? []
      : [
          {
            command: `pnpm exec vite preview --port ${webPort} --strictPort`,
            url: `http://localhost:${webPort}/`,
            env: { CONSOLE_API: rpc },
            reuseExistingServer: !process.env.CI,
          },
        ]),
    {
      // auth.mode=required, in-memory: probes stay open (readyz is the ready signal),
      // every RPC wants X-API-Key or a bearer JWT; the admin role comes from the
      // admin key or a token's roles claim, ops / a role-less token have none.
      command: `${bin}/rpc`,
      url: `${authRpc}/api/readyz`,
      env: {
        ...baseEnv,
        APP_HTTP_ADDR: `:${authRpcPort}`,
        APP_AUTH_MODE: "required",
        APP_AUTH_API_KEYS: `ops=${OPS_KEY},admin=${ADMIN_KEY}:admin`,
        APP_AUTH_JWT_HS256_SECRET: JWT_SECRET,
        APP_AUTH_JWT_ISSUER: JWT_ISSUER,
        APP_AUTH_JWT_AUDIENCE: JWT_AUDIENCE,
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
    {
      // auth.admin_role=operator: RequireRole wants "operator", not the default "admin".
      command: `${bin}/rpc`,
      url: `${roleRpc}/api/readyz`,
      env: {
        ...baseEnv,
        APP_HTTP_ADDR: `:${roleRpcPort}`,
        APP_AUTH_MODE: "required",
        APP_AUTH_ADMIN_ROLE: "operator",
        APP_AUTH_API_KEYS: `boss=${OPERATOR_KEY}:operator,legacy=${NOT_OPERATOR_KEY}:admin`,
      },
      reuseExistingServer: !process.env.CI,
      stdout: "ignore",
      stderr: "pipe",
    },
    {
      command: `pnpm exec vite preview --port ${roleWebPort} --strictPort`,
      url: `http://localhost:${roleWebPort}/`,
      env: { CONSOLE_API: roleRpc },
      reuseExistingServer: !process.env.CI,
    },
  ],
});
