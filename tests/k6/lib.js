// Shared by the k6 scripts: one Connect RPC over JSON, tagged by method so the
// thresholds in each script can be per RPC. Environment:
//   BASE_URL  server root (default http://127.0.0.1:8080); RPCs live under /api
//   API_KEY   X-API-Key when the server runs with auth on (optional)
import http from "k6/http";
import { check } from "k6";
import { Counter, Trend } from "k6/metrics";

export const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";

// Connect answers an RPC with 200 + the response message, or a non-200 status
// and {"code": "<connect code>", "message": ...}; the x-error-code header is
// the platform's own code (request.rate_limited, request.timeout, a domain code).
export const rpcDuration = new Trend("rpc_duration", true);
export const rpcErrors = new Counter("rpc_errors");

export function rpc(method, body, extra = {}) {
  const headers = { "Content-Type": "application/json" };
  if (__ENV.API_KEY) headers["X-API-Key"] = __ENV.API_KEY;
  const res = http.post(`${BASE_URL}/api/${method}`, JSON.stringify(body), {
    headers,
    tags: { rpc: method.split("/")[1], ...extra.tags },
    timeout: extra.timeout || "30s",
  });
  rpcDuration.add(res.timings.duration, { rpc: method.split("/")[1] });
  return res;
}

export function ok(res, what) {
  const good = check(res, { [`${what} 200`]: (r) => r.status === 200 });
  if (!good) {
    rpcErrors.add(1, { rpc: what, code: errorCode(res) });
  }
  return good ? res.json() : null;
}

export function errorCode(res) {
  return (
    res.headers["X-Error-Code"] || (res.status === 200 ? "" : safeCode(res))
  );
}

function safeCode(res) {
  try {
    return res.json("code") || String(res.status);
  } catch (_) {
    return String(res.status);
  }
}

// setStock for setup(): SetStock is a read-modify-write under optimistic locking, so a
// worker touching the same SKU at that moment answers 409 Aborted + concurrent_update —
// "reload and retry", which for an absolute on_hand means: send it again.
export function setStock(i, onHand) {
  let res;
  for (let attempt = 0; attempt < 5; attempt++) {
    res = rpc("inventory.v1.InventoryService/SetStock", {
      sku: sku(i),
      on_hand: onHand,
    });
    if (res.status !== 409) break;
  }
  return ok(res, "SetStock");
}

export function sku(i) {
  return `k6-${__ENV.RUN_ID || "run"}-${i}`;
}
