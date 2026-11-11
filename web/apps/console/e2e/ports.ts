// Every port the e2e stack listens on, derived from the one pair the operator may set
// (E2E_RPC_PORT, E2E_WEB_PORT). playwright.config.ts builds its webServers from this, so there is
// one derivation, not a second copy that can drift (#383). Pure: no I/O.
export function derivePorts(env: Record<string, string | undefined> = process.env) {
  const rpcPort = Number(env.E2E_RPC_PORT ?? 18091);
  const webPort = Number(env.E2E_WEB_PORT ?? 4173);
  // +1 for the auth project; e2e/image.sh publishes the console container on 18093 (= rpcPort + 2).
  const authRpcPort = rpcPort + 1;
  const authWebPort = webPort + 1;
  return {
    rpcPort,
    webPort,
    authRpcPort,
    authWebPort,
  };
}
