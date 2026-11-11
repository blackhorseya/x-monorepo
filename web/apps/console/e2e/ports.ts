// Every port the e2e stack listens on, derived from the one pair the operator may set
// (E2E_RPC_PORT, E2E_WEB_PORT). playwright.config.ts builds its webServers from this and
// e2e/assert-clean.ts checks the same list after a run, so there is one derivation, not a second
// copy that can drift (#383). Pure: no I/O, importable from the config and from plain node.
export function derivePorts(env: Record<string, string | undefined> = process.env) {
  const rpcPort = Number(env.E2E_RPC_PORT ?? 18091);
  const webPort = Number(env.E2E_WEB_PORT ?? 4173);
  // +1 for the auth project; e2e/image.sh publishes the console container on 18093 (= rpcPort + 2).
  const authRpcPort = rpcPort + 1;
  const authWebPort = webPort + 1;
  const rpcPorts = [rpcPort, authRpcPort];
  const webPorts = [webPort, authWebPort];
  return {
    rpcPort,
    webPort,
    authRpcPort,
    authWebPort,
    /** Everything the stack (and the tests that start their own listener) can hold, for the cleanup check. */
    all: [...rpcPorts, ...webPorts],
  };
}
