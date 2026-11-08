// Every port the e2e stack listens on, derived from the one pair the operator may set
// (E2E_RPC_PORT, E2E_WEB_PORT). playwright.config.ts builds its webServers from this and
// e2e/assert-clean.ts checks the same list after a run, so there is one derivation, not a second
// copy that can drift (#383). Pure: no I/O, importable from the config and from plain node.
/** Prefix of the temp dir that holds the oidc-backend rpc's generated config (one per IdP port). */
export const oidcTmpPrefix = (idpPort: number) => `x-monorepo-e2e-oidc-${idpPort}-`;

export function derivePorts(env: Record<string, string | undefined> = process.env) {
  const rpcPort = Number(env.E2E_RPC_PORT ?? 18091);
  const webPort = Number(env.E2E_WEB_PORT ?? 4173);
  const authRpcPort = rpcPort + 1;
  const authWebPort = webPort + 1;
  // +10, not +2: e2e/image.sh publishes the console container on 18093 (= rpcPort + 2).
  const roleRpcPort = rpcPort + 10;
  const roleWebPort = webPort + 10;
  // The "oidc-backend" pair: a real rpc with auth.oidc on, its own loopback IdP (e2e/oidc-backend/idp.ts).
  const backendRpcPort = rpcPort + 20;
  const backendIdpPort = rpcPort + 30;
  const backendWebPort = webPort + 20;
  // The catalog-rpc each rpc talks to (so projects do not share products).
  const catalogPort = (rpcServerPort: number) => rpcServerPort + 40; // 18131, 18132, 18141, 18151, …
  const rpcPorts = [rpcPort, authRpcPort, roleRpcPort, backendRpcPort];
  const webPorts = [webPort, authWebPort, roleWebPort, backendWebPort];
  return {
    rpcPort,
    webPort,
    authRpcPort,
    authWebPort,
    roleRpcPort,
    roleWebPort,
    backendRpcPort,
    backendIdpPort,
    backendWebPort,
    catalogPort,
    /** The port the "other IdP" of the issuer-mismatch negative uses (e2e/oidc-backend/backend.spec.ts). */
    foreignIdpPort: backendIdpPort + 1,
    /** Everything the stack (and the tests that start their own listener) can hold, for the cleanup check. */
    all: [...rpcPorts, ...rpcPorts.map(catalogPort), backendIdpPort, backendIdpPort + 1, ...webPorts],
  };
}
