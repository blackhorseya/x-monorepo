import { readdirSync } from "node:fs";
import { createConnection } from "node:net";
import { tmpdir } from "node:os";
import { derivePorts, oidcTmpPrefix } from "./ports.ts";

// After a run: nothing the e2e stack started is still listening on a port it derived, and the
// per-run rpc config (e2e/oidc-backend/rpc-server.ts) is gone. Exit 1 lists what is left (#383).
const p = derivePorts();
const open = (port: number) =>
  new Promise<boolean>((resolve) => {
    const socket = createConnection({ port, host: "127.0.0.1" });
    socket.once("connect", () => (socket.destroy(), resolve(true)));
    socket.once("error", () => resolve(false));
  });

const listening = (await Promise.all(p.all.map(async (port) => ((await open(port)) ? port : 0)))).filter(Boolean);
const prefix = oidcTmpPrefix(p.backendIdpPort);
const leftovers = readdirSync(tmpdir()).filter((name) => name.startsWith(prefix));
if (listening.length || leftovers.length) {
  console.error(`e2e left behind: listening ports [${listening.join(", ")}], temp config dirs [${leftovers.join(", ")}] in ${tmpdir()}`);
  process.exit(1);
}
console.log(`e2e cleanup ok: ${p.all.length} derived ports closed, no temp config under ${tmpdir()}`);
