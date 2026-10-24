import { spawn, spawnSync } from "node:child_process";
import { withDB } from "./mode";

// DB mode only: runs bin/migrate up (idempotent; the rpc webServer does too, order
// between globalSetup and webServer is not something to lean on) and starts one
// bin/worker on the same Postgres so orders get reservations. The worker has no
// HTTP port to wait on, so specs that need it poll with a generous timeout.
// Consumer group is the default ("worker"): a fixed cursor, no stale group left
// behind for prune / the eventbus alerts to complain about.
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

  const worker = spawn(`${bin}/worker`, [], { env, stdio: ["ignore", "ignore", "inherit"] });
  const exited = new Promise<number | null>((resolve) => worker.once("exit", resolve));
  worker.once("exit", (code) => {
    if (code !== 0 && code !== null) console.error(`[worker] exited with ${code}`);
  });

  return async () => {
    worker.kill("SIGTERM"); // drains in-flight handlers (≤ 8s), then exits 0
    await Promise.race([exited, new Promise((r) => setTimeout(r, 15_000))]);
    if (worker.exitCode === null) worker.kill("SIGKILL");
  };
}
