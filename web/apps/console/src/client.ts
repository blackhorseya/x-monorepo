import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AdminService } from "@repo/proto/admin/v1/service_pb";
import { CatalogService } from "@repo/proto/catalog/v1/service_pb";
import { IdentityService } from "@repo/proto/identity/v1/service_pb";
import { InventoryService } from "@repo/proto/inventory/v1/service_pb";
import { OrderService } from "@repo/proto/ordering/v1/service_pb";
import { ViolationsSchema } from "@repo/proto/buf/validate/violations_pb";
import { credentialHeaders, getCredential, getCredentialGeneration } from "./auth";
import { getConfig } from "./oidc/config";
import { canRefresh, refreshNow } from "./oidc/session";
import { UNBOUND_CODE, markUnbound } from "./oidc/unbound";

// Same origin, under /api: vite proxies /api to the Go server, and in production
// one ingress rule for /api covers RPCs, docs, healthz and metrics alike.
// The credential interceptor attaches X-API-Key / Authorization from src/auth.ts
// to every call (nothing when anonymous), read at call time so a change in
// the header control applies to the next RPC without rebuilding clients.
const withCredential: Interceptor = (next) => (req) => {
  for (const [k, v] of Object.entries(credentialHeaders())) req.header.set(k, v);
  return next(req);
};
// An access token the server turns down (expired between refreshes, the tab was asleep) is renewed
// once and the call retried; if the refresh does not give a new token the original error stands.
// Only a call that was sent with the session's own token qualifies (an anonymous call that fails
// after sign-in says nothing about the token), and one that lost the race to a refresh already in
// the books is just retried with the newer token instead of asking the IdP again.
const renewOnUnauthenticated: Interceptor = (next) => async (req) => {
  const generation = getCredentialGeneration();
  const sent = getCredential()?.value;
  const eligible = canRefresh();
  try {
    return await next(req);
  } catch (err) {
    if (ConnectError.from(err).code !== Code.Unauthenticated || !eligible || generation !== getCredentialGeneration()) throw err;
    if (getCredential()?.value === sent && (await refreshNow()) !== "ok") throw err;
    return next(req);
  }
};
// A verified token the server has no account for: remember it for the credential it was seen with,
// so the layout can offer ClaimInvite instead of pages that can only fail.
const detectUnbound: Interceptor = (next) => async (req) => {
  const generation = getCredentialGeneration();
  try {
    return await next(req);
  } catch (err) {
    const e = ConnectError.from(err);
    if (getConfig().oidc && e.code === Code.PermissionDenied && e.metadata.get("x-error-code") === UNBOUND_CODE) markUnbound(generation);
    throw err;
  }
};
const transport = createConnectTransport({
  baseUrl: `${window.location.origin}/api`,
  interceptors: [detectUnbound, renewOnUnauthenticated, withCredential],
});

export const orders = createClient(OrderService, transport);
export const inventory = createClient(InventoryService, transport);
export const admin = createClient(AdminService, transport);
export const identity = createClient(IdentityService, transport);
export const catalog = createClient(CatalogService, transport);

// describe turns a Connect error into one line: the domain code from the
// x-error-code header, plus protovalidate field violations when present.
export function describe(err: unknown): string {
  const e = ConnectError.from(err);
  const code = e.metadata.get("x-error-code") ?? Code[e.code];
  const violations = e
    .findDetails(ViolationsSchema)
    .flatMap((v) => v.violations)
    .map((v) => `${fieldPath(v.field?.elements ?? [])}: ${v.message}`);
  return violations.length > 0 ? `${code} — ${violations.join("; ")}` : `${code} — ${e.rawMessage}`;
}

function fieldPath(elements: { fieldName?: string; subscript: { case?: string; value?: unknown } }[]): string {
  return elements
    .map((el) => (el.subscript.case ? `${el.fieldName}[${String(el.subscript.value)}]` : el.fieldName))
    .join(".");
}

export function isNotFound(err: unknown): boolean {
  return ConnectError.from(err).code === Code.NotFound;
}

// The server runs with auth on and did not accept what we sent (or nothing).
export function isUnauthenticated(err: unknown): boolean {
  return ConnectError.from(err).code === Code.Unauthenticated;
}
