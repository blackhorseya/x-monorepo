import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { InventoryService } from "@repo/proto/inventory/v1/service_pb";
import { OrderService } from "@repo/proto/ordering/v1/service_pb";
import { ViolationsSchema } from "@repo/proto/buf/validate/validate_pb";
import { credentialHeaders } from "./auth";

// Same origin, under /api: vite proxies /api to the Go server, and in production
// one ingress rule for /api covers RPCs, docs, healthz and metrics alike.
// The credential interceptor attaches X-API-Key / Authorization from src/auth.ts
// to every call (nothing when anonymous), read at call time so a change in
// the header control applies to the next RPC without rebuilding clients.
const withCredential: Interceptor = (next) => (req) => {
  for (const [k, v] of Object.entries(credentialHeaders())) req.header.set(k, v);
  return next(req);
};
const transport = createConnectTransport({
  baseUrl: `${window.location.origin}/api`,
  interceptors: [withCredential],
});

export const orders = createClient(OrderService, transport);
export const inventory = createClient(InventoryService, transport);

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
