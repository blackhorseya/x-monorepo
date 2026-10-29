// Shared by the k6 scripts: one Connect RPC over JSON, tagged by method so the
// thresholds in each script can be per RPC. Environment:
//   BASE_URL  server root (default http://127.0.0.1:8080); RPCs live under /api
//   API_KEY   X-API-Key when the server runs with auth on (optional)
//   K6_CUSTOMER_MODE  who names the customer (default auto), see customerId below
//             Ordering is owned by the caller (docs/identity.md): a server that knows the
//             caller (a key or JWT) takes the customer from it, one that does not (auth off,
//             even with a key sent; a public procedure) needs customer_id.
import http from "k6/http";
import exec from "k6/execution";
import { KNOWN_CODES } from "./codes.js";
import { check } from "k6";
import { Counter, Trend } from "k6/metrics";

export const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";

// Connect answers an RPC with 200 + the response message, or a non-200 status
// and {"code": "<connect code>", "message": ...}; the x-error-code header is
// the platform's own code (request.rate_limited, request.timeout, a domain code).
export const rpcDuration = new Trend("rpc_duration", true);
export const rpcErrors = new Counter("rpc_errors");
// Successful operations per RPC: the scripts threshold on it (count>0) so a run that was
// refused everywhere cannot pass on "no errors" alone, and cells of the auth gate are comparable.
export const rpcOk = new Counter("rpc_ok");

export function rpc(method, body, extra = {}) {
  const headers = { "Content-Type": "application/json" };
  if (__ENV.API_KEY) headers["X-API-Key"] = __ENV.API_KEY;
  const res = http.post(`${BASE_URL}/api/${method}`, JSON.stringify(body), {
    headers,
    tags: { rpc: method.split("/")[1], ...extra.tags },
    timeout: extra.timeout || "30s",
    responseCallback: extra.responseCallback,
  });
  // a probe's expected refusal is not a sample of the RPC's latency
  if (!extra.probe) rpcDuration.add(res.timings.duration, { rpc: method.split("/")[1] });
  return res;
}

// Who names the customer is the server's answer to the request itself, not something to infer
// from the environment (an X-API-Key that is sent but ignored by auth off), nor from another
// procedure (WhoAmI is public while CreateOrder is not; one call that fails says nothing about
// the rest). K6_CUSTOMER_MODE:
//   auto     (default) the first CreateOrder of a VU is sent without customer_id. 200 → the
//            server knows the caller: name no customer from now on. 400 order.empty_customer →
//            it does not: send the script's own customer (this call is repeated with it, nothing
//            was written). Only these two answers are remembered; anything else (503, 401, 403, a
//            refused credential…) is not cached: the run aborts at once with that answer.
//   include  always send the script's customer_id (auth off; a key that may place for others)
//   omit     never send it (the server takes the caller's subject)
// ListOrders follows what CreateOrder resolved: auth off filters by the script's customer, a
// principal sees its own (an admin: all). Per VU, as k6 runs each VU in its own runtime.
const MODE = __ENV.K6_CUSTOMER_MODE || "auto";
if (!["auto", "include", "omit"].includes(MODE)) {
  throw new Error(`K6_CUSTOMER_MODE=${MODE}: want auto, include or omit`);
}
let resolved = MODE === "auto" ? "" : MODE;

export function customerId(name) {
  if (!resolved) {
    exec.test.abort(
      "customer probe: ListOrders before CreateOrder resolved who names the customer (set K6_CUSTOMER_MODE)"
    );
  }
  return resolved === "include" ? { customer_id: name } : {};
}

// createOrder is CreateOrder with the customer as K6_CUSTOMER_MODE says; the response is
// returned as rpc() does, for ok().
export function createOrder(body, name) {
  const method = "ordering.v1.OrderService/CreateOrder";
  if (resolved) return rpc(method, { ...body, ...customerId(name) });
  const res = rpc(method, body, {
    probe: true,
    tags: { probe: "true" },
    responseCallback: http.expectedStatuses(200, 400),
  });
  if (res.status === 200) {
    resolved = "omit";
    return res;
  }
  if (res.status === 400 && errorCode(res) === "order.empty_customer") {
    resolved = "include";
    return rpc(method, { ...body, ...customerId(name) });
  }
  exec.test.abort(
    redact(
      `customer probe: ${method} answered status ${res.status} code ${errorCode(res)}; ` +
        "not cached, not a customer answer (K6_CUSTOMER_MODE=include|omit skips the probe)"
    )
  );
}

let shown = 0; // per VU
export function ok(res, what) {
  const good = check(res, { [`${what} 200`]: (r) => r.status === 200 });
  if (!good) {
    rpcErrors.add(1, { rpc: what, code: errorCode(res) });
    // the counter tags are not in the summary; say which RPC was refused, and how, a few times
    if (shown++ < 3) {
      console.error(redact(`${what}: status ${res.status} code ${errorCode(res)}`));
    }
  } else {
    rpcOk.add(1, { rpc: what });
  }
  return good ? res.json() : null;
}

// What a response says about itself, for logs, messages and tags: the Connect / x-error-code
// value only, and only if it is a known one (codes.js); else <unrecognized>. The body and any other
// header are never echoed — a misbehaving upstream or proxy can reflect the request's X-API-Key
// into them, into a code that merely looks like a code included. A response without any code is
// its HTTP status.
export function errorCode(res) {
  if (res.status === 200) return "";
  const code = res.headers["X-Error-Code"] || jsonCode(res);
  if (!code) return String(res.status);
  return KNOWN_CODES.has(code) ? code : "<unrecognized>";
}

function jsonCode(res) {
  try {
    return res.json("code") || "";
  } catch (_) {
    return "";
  }
}

// redact is the second layer: no diagnostic leaves with the key this run uses in it.
export function redact(text) {
  const key = __ENV.API_KEY;
  return key ? String(text).split(key).join("<redacted>") : String(text);
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

// customer name for a script's own customers (auth off, or a server that does not know the
// caller): RUN_ID in it, so runs sharing a database do not share customers.
export function customer(label) {
  return `k6-${__ENV.RUN_ID || "run"}-${label}`;
}

export function sku(i) {
  return `k6-${__ENV.RUN_ID || "run"}-${i}`;
}
