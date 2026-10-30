import { TestIdp } from "./idp.ts";

// Runs the loopback IdP as its own process (a Playwright webServer), so it is up before the rpc
// starts and the rpc's JWKS / discovery fetches succeed from the first request.
const port = Number(process.env.IDP_PORT);
const idp = new TestIdp(port, process.env.IDP_CLIENT_ID ?? "x-console-e2e");
await idp.start();
console.log(`test idp on ${idp.issuer}`);
