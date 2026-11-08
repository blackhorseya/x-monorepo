import { spawnSync } from "node:child_process";
import { withDB } from "./mode";

// DB mode only: runs bin/migrate up (idempotent; the rpc webServer does too, order
// between globalSetup and webServer is not something to lean on). No bounded context
// consumes events, so there is no worker to start.
const bin = "../../../bin";

export default async function setup() {
  if (!withDB) return;
  const env = {
    ...process.env,
    APP_CONFIG: "/dev/null",
    APP_POSTGRES_URL: process.env.E2E_DATABASE_URL,
    APP_LOG_LEVEL: process.env.E2E_WORKER_LOG_LEVEL ?? "warn",
  };
  const migrate = spawnSync(`${bin}/migrate`, ["up"], { env, stdio: ["ignore", "ignore", "inherit"] });
  if (migrate.status !== 0) throw new Error(`migrate up exited ${migrate.status}`);
}
