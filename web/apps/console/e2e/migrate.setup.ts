import { spawnSync } from "node:child_process";
import { withDB } from "./mode";

// DB mode only: always runs bin/migrate up (idempotent). The rpc webServer command runs it
// too, except when an existing server is reused (reuseExistingServer, local only: the
// command, migrate included, is skipped). Kept anyway: Playwright's docs do not promise an
// order between globalSetup and webServer, and on a reused server this is the only migrate.
const bin = "../../../bin";

export default async function setup() {
  if (!withDB) return;
  const env = {
    ...process.env,
    APP_CONFIG: "/dev/null",
    APP_POSTGRES_URL: process.env.E2E_DATABASE_URL,
    APP_LOG_LEVEL: "warn", // same as baseEnv in playwright.config.ts
  };
  const migrate = spawnSync(`${bin}/migrate`, ["up"], { env, stdio: ["ignore", "ignore", "inherit"] });
  if (migrate.status !== 0) throw new Error(`migrate up exited ${migrate.status}`);
}
