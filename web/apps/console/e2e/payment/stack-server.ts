import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createServer } from "node:http";
import { BIN, ADMIN_KEY, CATALOG_KEY, PAYMENT_KEY, SHOPPER, SHOPPER_KEY, WEBHOOK_SECRET, cleanEnv, databaseUrls, paymentPorts } from "./env.ts";
import { derivePorts } from "../ports.ts";

// The stack of the "payment" project, as ONE process Playwright starts (webServer, SIGTERM on the way out):
// it makes a database of its own on the server E2E_DATABASE_URL names, brings migrate up, and runs
//   catalog-rpc, payment-rpc serve, payment-worker, rpc                  (always)
//   worker                                                              (started and switched through the control port)
// each a real binary of this build. Nothing is scripted: the order is created in the console, reserved by the
// worker, confirmed under the snapshot the worker's switch says, paid through payment-rpc's own mock control, and
// completed by ordering consuming PaymentCaptured.
//
// The worker is the one process a test restarts: ordering.require_payment is read by the worker when an order
// becomes CONFIRMED (it takes the payment_required snapshot), so "switch it back" is a new worker with the switch
// off. The control port is how: POST /worker?require_payment=true|false (and DELETE /worker to stop it).
// GET /ready answers 200 once every always-on process answered.
//
// Cleanup is this file's job: SIGTERM stops the children (SIGKILL after a grace), drops the database and closes
// the control port; e2e/assert-clean.ts checks every port afterwards.

const adminUrl = process.env.E2E_DATABASE_URL;
if (!adminUrl) throw new Error("the payment project needs E2E_DATABASE_URL (a Postgres server to make its own database on)");
const P = derivePorts();
const ports = paymentPorts();
const db = databaseUrls(adminUrl, P.rpcPort);
const base = { APP_POSTGRES_URL: db.stack } as const;
const children = new Map<string, ChildProcess>();
let stopping = false;

const psql = (sql: string) => {
  const r = spawnSync("psql", [db.admin, "-v", "ON_ERROR_STOP=1", "-q", "-c", sql], { stdio: ["ignore", "inherit", "inherit"] });
  if (r.status !== 0) throw new Error(`psql ${sql}: exit ${r.status ?? r.signal}`);
};

function start(name: string, bin: string, args: string[], env: Record<string, string>) {
  const child = spawn(bin, args, { env: cleanEnv({ ...base, ...env }), stdio: ["ignore", "inherit", "inherit"] });
  children.set(name, child);
  child.once("exit", (code, signal) => {
    children.delete(name);
    if (!stopping && name !== "worker") {
      console.error(`[stack] ${name} exited early (${code ?? signal})`);
      void shutdown(1);
    }
  });
  child.once("error", (err) => {
    console.error(`[stack] ${name}: ${err}`);
    void shutdown(1);
  });
  return child;
}

async function stop(name: string, timeoutMs = 10_000) {
  const child = children.get(name);
  if (!child) return;
  const exited = new Promise<void>((resolve) => child.once("exit", () => resolve()));
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), timeoutMs);
  await exited;
  clearTimeout(timer);
}

async function ready(url: string, what: string, ms = 60_000) {
  const deadline = Date.now() + ms;
  for (;;) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) throw new Error(`${what} never answered ${url}`);
    await new Promise((r) => setTimeout(r, 200));
  }
}

const serveUrl = `http://127.0.0.1:${ports.serve}`;
const paymentEnv = {
  APP_PAYMENT_PROVIDER: "mock",
  APP_PAYMENT_MOCK_CONTROL: "true",
  APP_PAYMENT_MOCK_WEBHOOK_SECRET: WEBHOOK_SECRET,
  APP_PAYMENT_EXPIRE_INTERVAL: "15m",
};

function startWorker(requirePayment: boolean) {
  return start("worker", BIN.worker, [], {
    APP_WORKER_CONSUMER_GROUP: "e2e-payment-worker",
    APP_WORKER_POLL_INTERVAL: "100ms",
    ...(requirePayment ? { APP_ORDERING_REQUIRE_PAYMENT: "true", APP_PAYMENT_UPSTREAM: serveUrl } : {}),
  });
}

let allUp = false;
const control = createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", "http://x");
  const say = (code: number, body = "") => {
    res.statusCode = code;
    res.end(body);
  };
  try {
    if (req.method === "GET" && url.pathname === "/ready") return say(allUp ? 200 : 503);
    if (url.pathname === "/worker" && req.method === "POST") {
      const want = url.searchParams.get("require_payment");
      if (want !== "true" && want !== "false") return say(400, "require_payment=true|false");
      await stop("worker");
      startWorker(want === "true");
      return say(200);
    }
    if (url.pathname === "/worker" && req.method === "DELETE") {
      await stop("worker");
      return say(200);
    }
    return say(404);
  } catch (err) {
    console.error(`[stack] control: ${err}`);
    return say(500);
  }
});

let shuttingDown: Promise<void> | undefined;
function shutdown(code: number) {
  shuttingDown ??= (async () => {
    stopping = true;
    for (const name of [...children.keys()]) await stop(name);
    try {
      psql(`DROP DATABASE IF EXISTS ${db.name} WITH (FORCE)`);
    } catch (err) {
      console.error(`[stack] drop database: ${err}`);
      code = code || 1;
    }
    control.close();
    process.exit(code);
  })();
  return shuttingDown;
}
process.on("SIGTERM", () => void shutdown(0));
process.on("SIGINT", () => void shutdown(0));

psql(`DROP DATABASE IF EXISTS ${db.name} WITH (FORCE)`);
psql(`CREATE DATABASE ${db.name}`);
const migrate = spawnSync(BIN.migrate, ["up"], { env: cleanEnv(base), stdio: ["ignore", "inherit", "inherit"] });
if (migrate.status !== 0) {
  console.error(`[stack] migrate up exited ${migrate.status}`);
  await shutdown(1);
}

control.listen(ports.control, "127.0.0.1");
start("catalog-rpc", BIN.catalog, [], { APP_HTTP_ADDR: `:${ports.catalog}`, APP_AUTH_MODE: "required", APP_AUTH_API_KEYS: `rpc=${CATALOG_KEY}:admin` });
start("payment-serve", BIN.payment, ["serve"], { ...paymentEnv, APP_HTTP_ADDR: `:${ports.serve}`, APP_AUTH_MODE: "required", APP_AUTH_API_KEYS: `rpc=${PAYMENT_KEY}:admin` });
start("payment-consume", BIN.paymentWorker, [], { ...paymentEnv, APP_WORKER_CONSUMER_GROUP: "payment-rpc", APP_WORKER_POLL_INTERVAL: "100ms" });
start("rpc", BIN.rpc, [], {
  APP_HTTP_ADDR: `:${ports.rpc}`,
  APP_AUTH_MODE: "required",
  APP_AUTH_API_KEYS: `${SHOPPER}=${SHOPPER_KEY},admin=${ADMIN_KEY}:admin`,
  APP_CATALOG_UPSTREAM: `http://127.0.0.1:${ports.catalog}`,
  APP_CATALOG_API_KEY: CATALOG_KEY,
  APP_PAYMENT_UPSTREAM: serveUrl,
  APP_PAYMENT_API_KEY: PAYMENT_KEY,
  APP_ORDERING_REQUIRE_PAYMENT: "true",
});
startWorker(true);

await Promise.all([
  ready(`http://127.0.0.1:${ports.catalog}/api/readyz`, "catalog-rpc"),
  ready(`${serveUrl}/api/readyz`, "payment-rpc serve"),
  ready(`http://127.0.0.1:${ports.rpc}/api/readyz`, "rpc"),
]).catch(async (err) => {
  console.error(`[stack] ${err}`);
  await shutdown(1);
});
allUp = true;
console.error(`[stack] up: rpc :${ports.rpc}, payment-rpc serve :${ports.serve}, database ${db.name}`);
