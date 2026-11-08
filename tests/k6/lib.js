// Shared by the k6 scripts: one Connect RPC over JSON, tagged by method so the
// thresholds in each script can be per RPC. Environment:
//   BASE_URL  server root (default http://127.0.0.1:8080); RPCs live under /api
//   API_KEY   X-API-Key when the server runs with auth on (optional)
import http from "k6/http";
import { KNOWN_CODES } from "./codes.js";
import { Trend } from "k6/metrics";

export const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";

// Connect answers an RPC with 200 + the response message, or a non-200 status
// and {"code": "<connect code>", "message": ...}; the x-error-code header is
// the platform's own code (request.rate_limited, request.timeout, a domain code).
// options.systemTags of every script: k6's defaults without url (a request's URL is the one
// place a followed redirect or a mistaken BASE_URL could put a secret into every sample) and
// without error (the transport's own message, free text: Go's HTTP parser quotes the status
// code, version or header line it could not parse, which a misbehaving upstream or proxy can
// make the request's X-API-Key). error_code, k6's fixed numeric class of the failure, stays.
export const systemTags = [
  "proto", "subproto", "status", "method", "name", "group", "check",
  "error_code", "tls_version", "scenario", "service", "expected_response",
];

// options.throw of every script: with it a failed request is an exception thrown to rpc()
// below, which says what happened in a fixed vocabulary. Without it k6 prints the raw error
// ("Request Failed" warning) on its own before the script sees the response — a try/catch
// cannot come first. Every http call must therefore go through rpc(): an exception that
// escapes is printed by k6 with its message.
export const throwOnError = true;

export const rpcDuration = new Trend("rpc_duration", true);

let unanswered = 0; // per VU
export function rpc(method, body, extra = {}) {
  const headers = { "Content-Type": "application/json" };
  const key = extra.apiKey || __ENV.API_KEY;
  if (key) headers["X-API-Key"] = key;
  let res;
  try {
    res = http.post(`${BASE_URL}/api/${method}`, JSON.stringify(body), {
      headers,
      // name = the RPC, not the URL; redirects 0: a 3xx is an answer to report, never followed (the
      // Location is the server's to choose, and a followed URL is logged and tagged)
      tags: { name: method, rpc: method.split("/")[1], ...extra.tags },
      redirects: 0,
      timeout: extra.timeout || "30s",
      responseCallback: extra.responseCallback,
    });
  } catch (e) {
    // the exception's message is the raw transport error: classified, never echoed
    const failed = transportFailure(e);
    // decisions use knownCode(); output goes through redact() like errorCode() does (a key can equal a class)
    if (unanswered++ < 3) console.error(redact(`${method}: no response, ${knownCode(failed)}`));
    return failed;
  }
  // a probe's expected refusal is not a sample of the RPC's latency
  if (!extra.probe) rpcDuration.add(res.timings.duration, { rpc: method.split("/")[1] });
  return res;
}

// transportFailure is what rpc() returns when no response came back (status 0, as k6 itself
// reports one): the shape the scripts read, with the failure as one of a fixed set of classes
// (knownCode → transport.<class>). Not a sample of rpc_duration: there was no answer to time.
function transportFailure(e) {
  return {
    status: 0,
    headers: {},
    body: "",
    error_code: 1000,
    transport: transportClass(e),
    timings: { duration: 0 },
    json: () => undefined,
  };
}

// transportClass reads the error only to pick a class; the text itself goes nowhere. The first
// match wins: Go wraps a parser failure in "connection broken", so malformed comes before eof.
function transportClass(e) {
  const m = String((e && e.message) || e);
  if (/malformed|invalid|missing colon|bad |illegal|too (long|large)/i.test(m)) return "malformed_response";
  if (/unexpected eof/i.test(m)) return "truncated_response";
  if (/timeout|deadline/i.test(m)) return "timeout";
  if (/refused/i.test(m)) return "connection_refused";
  if (/no such host|lookup/i.test(m)) return "dns";
  if (/\beof\b|reset|broken pipe|closed/i.test(m)) return "connection_closed";
  return "other";
}

// What a response says about itself, for logs, messages and tags: the Connect / x-error-code
// value only, and only if it is a known one (codes.js); else <unrecognized>. A request that got
// no response at all is transport.<class> (malformed_response, truncated_response, timeout,
// connection_refused, dns, connection_closed, other). The body and any other
// header are never echoed — a misbehaving upstream or proxy can reflect the request's X-API-Key
// into them, into a code that merely looks like a code included. A response without any code is
// its HTTP status.
export function errorCode(res) {
  // the one exit for every output (abort, tag, console): known value only, redacted if the key
  // happens to be one
  return redact(knownCode(res));
}

// knownCode is for decisions (the empty_customer split, the 429 / 504 checks), never printed or
// tagged: unredacted. errorCode() is for output.
export function knownCode(res) {
  if (res.status === 200) return "";
  if (res.status === 0 && res.transport) return `transport.${res.transport}`;
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
  let out = String(text);
  for (const key of [__ENV.API_KEY]) {
    if (key) out = out.split(key).join("<redacted>");
  }
  return out;
}
