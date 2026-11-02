import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { oidcTmpPrefix } from "../ports.ts";

// The rpc of the "oidc-backend" project. Its only OIDC issuer must be the loopback IdP, whose
// address is derived from E2E_RPC_PORT (e2e/ports.ts); a config FILE is unavoidable (auth.oidc.issuers
// is a list, which cannot come from env), so this wrapper writes it from the template beside it, runs
// bin/rpc on it, and removes it again. The config lives and dies with this process: a run on other
// ports gets its own, and a stopped run leaves none behind (playwright.config.ts asks for SIGTERM, not
// SIGKILL, so the cleanup below runs).
const idpPort = Number(process.env.IDP_PORT);
if (!Number.isInteger(idpPort) || idpPort < 1 || idpPort > 65535) throw new Error(`IDP_PORT=${process.env.IDP_PORT} is not a port`);
const rpcBin = process.env.RPC_BIN ?? "../../../bin/rpc";

const template = readFileSync(new URL("./rpc.yaml", import.meta.url), "utf8");
const config = template.replaceAll("@IDP_PORT@", String(idpPort));
if (config.includes("@")) throw new Error("rpc.yaml still has an unfilled placeholder");

// Per-port prefix: e2e/assert-clean.ts looks for exactly this after a run.
const dir = mkdtempSync(join(tmpdir(), oidcTmpPrefix(idpPort)));
const file = join(dir, "rpc.yaml");
writeFileSync(file, config, { mode: 0o600 });

const child = spawn(rpcBin, [], { env: { ...process.env, APP_CONFIG: file }, stdio: ["ignore", "inherit", "inherit"] });
let finished = false;
const cleanup = () => {
  if (finished) return;
  finished = true;
  rmSync(dir, { recursive: true, force: true });
};
process.on("exit", cleanup);

const stop = (signal: NodeJS.Signals) => {
  if (child.exitCode === null && child.signalCode === null) {
    child.kill(signal);
    // A child that ignores the signal must not outlive the run.
    setTimeout(() => child.kill("SIGKILL"), 4_000).unref();
  }
};
process.on("SIGTERM", () => stop("SIGTERM"));
process.on("SIGINT", () => stop("SIGINT"));

child.once("exit", (code, signal) => {
  cleanup();
  process.exit(code ?? (signal ? 128 : 1));
});
child.once("error", (err) => {
  console.error(err);
  cleanup();
  process.exit(1);
});
