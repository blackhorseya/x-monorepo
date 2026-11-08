import type { Page, Route } from "@playwright/test";

// Fault injection for the console's RPCs, aimed at one RPC method (and optionally one caller): for
// specs that need a server failure or a slow answer at a precise moment. Every other request goes
// on untouched, and the handlers hand it to the next one (route.fallback), so several of these on
// one page, and a spec's own page.route, compose. Counters are per call.
//
// method  the RPC's name, the last segment of the URL ("ListAccounts", "QuotePrices"); matched exactly.
// key     which callers: omitted = any; a string = calls sent with that X-API-Key; null = calls sent
//         with no X-API-Key (the anonymous ones: the console's first requests before a credential is set).
// bodyIncludes  omitted = any body; a string = only calls whose request body contains it (substring, case
//         sensitive). For a page that sends the same RPC about something else first: a page may ask its
//         default SKU before the one a test types, and a fault meant for the typed SKU must not be spent on that.
export type Target = { method: string; key?: string | null; bodyIncludes?: string };

function matches({ method, key, bodyIncludes }: Target) {
  return (url: string, headers: Record<string, string>, body: string | null) => {
    const path = new URL(url).pathname;
    if (path.slice(path.lastIndexOf("/") + 1) !== method) return false;
    if (bodyIncludes !== undefined && !(body ?? "").includes(bodyIncludes)) return false;
    if (key === undefined) return true;
    const sent = headers["x-api-key"];
    return key === null ? sent === undefined : sent === key;
  };
}

// ---- failRequests: the next `times` matching calls fail -------------------------------------
//
//   const fault = await failRequests(page, { method: "ListAccounts", key: OPS_KEY });   // before page.goto
//   ...
//   await expect.poll(() => fault.failed()).toBe(1);   // the fault was really hit: no vacuous run
//
// times   how many matching calls fail (default 1); the ones after that pass through.
// mode    "unavailable" (default) answers 503 with a Connect `unavailable` error; "reset" drops the
//         connection (the browser sees "Failed to fetch"); "permission_denied" answers 403 with a Connect
//         `permission_denied` error, "invalid_argument" answers 400 with a Connect `invalid_argument` error: answers,
//         not blips, for specs that must show they are not retried.
// (times: Infinity = every matching call fails.)
export type FailRequestsOptions = Target & { times?: number; mode?: "unavailable" | "reset" | "permission_denied" | "invalid_argument" };

export type RequestFault = {
  /** Calls that matched (method and key), failed or not. */
  matched: () => number;
  /** Calls that were failed. */
  failed: () => number;
};

export async function failRequests(page: Page, options: FailRequestsOptions): Promise<RequestFault> {
  const { times = 1, mode = "unavailable" } = options;
  const hit = matches(options);
  let matched = 0;
  let failed = 0;
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (!hit(request.url(), request.headers(), request.postData())) return route.fallback();
    matched++;
    if (failed >= times) return route.fallback();
    failed++;
    if (mode === "reset") return route.abort("connectionreset");
    return route.fulfill({
      status: { unavailable: 503, permission_denied: 403, invalid_argument: 400 }[mode],
      contentType: "application/json",
      body: JSON.stringify({ code: mode, message: "injected by failRequests" }),
    });
  });
  return { matched: () => matched, failed: () => failed };
}

// ---- holdRequests: matching calls are parked until release() ---------------------------------
//
//   const held = await holdRequests(page, { method: "ListAccounts", key: null });   // before page.goto
//   await page.goto("/accounts");
//   await held.arrived();          // the anonymous ListAccounts is on the wire, unanswered
//   await useKey(page, OPS_KEY);   // the credential changes while the first answer is still out
//   held.release();                // now the stale answer may land
//
// A parked call is not sent to the server until release(); after release() matching calls pass straight
// through and are not counted: arrived() and count() only ever speak of calls that really were parked, so
// they are safe as a positive control ("the hold was hit") even when release() runs in a cleanup path.
// Several calls can be parked at once.
export type RequestHold = {
  /** Resolves once the first matching call is parked; never resolves if none was before release(). */
  arrived: () => Promise<void>;
  /** Calls parked so far. */
  count: () => number;
  /** Let every parked call, and every later one, go to the server. */
  release: () => void;
};

export async function holdRequests(page: Page, target: Target): Promise<RequestHold> {
  const hit = matches(target);
  let count = 0;
  let released = false;
  let open!: () => void;
  const gate = new Promise<void>((r) => (open = r));
  const release = () => {
    released = true;
    open();
  };
  let arrive!: () => void;
  const arrival = new Promise<void>((r) => (arrive = r));
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (!hit(request.url(), request.headers(), request.postData()) || released) return route.fallback();
    count++;
    arrive();
    await gate;
    return route.fallback();
  });
  return { arrived: () => arrival, count: () => count, release };
}

// ---- scriptByKey: each caller gets its own scripted answer --------------------------------------
//
//   const list = await scriptByKey(page, "identity.v1.IdentityService/ListAccounts", { "": { connect: "unauthenticated" }, OPS: { ok: {...} } });
//
// rpc   service and method as they end the URL ("identity.v1.IdentityService/ListAccounts"); matched exactly.
// plan  the Reply for a call sent with that X-API-Key ("" = no X-API-Key). A key that is not in the plan is
//       a test bug and is answered HTTP 599 (not passed on) so it cannot go unnoticed.
// It answers the call itself, so a hold meant to park the call is registered AFTER it (the newest handler
// runs first): holdRequests parks the call and, on release(), hands it on to this script.
// count(key) = calls that reached this script, answered or refused with 599: a call a hold has parked has
// not reached it yet.
export type Reply = { ok: object } | { connect: "unauthenticated" | "permission_denied" | "unavailable" } | { http: number };

const answer = (r: Reply) => {
  if ("ok" in r) return { status: 200, contentType: "application/json", body: JSON.stringify(r.ok) };
  if ("http" in r) return { status: r.http, contentType: "text/plain", body: "injected" };
  const status = { unauthenticated: 401, permission_denied: 403, unavailable: 503 }[r.connect];
  return { status, contentType: "application/json", body: JSON.stringify({ code: r.connect, message: `injected: ${r.connect}` }) };
};

export async function scriptByKey(page: Page, rpc: string, plan: Record<string, Reply>) {
  const seen: string[] = []; // the key of every call that reached this script, in order
  await page.route(`**/${rpc}`, async (route: Route) => {
    const key = route.request().headers()["x-api-key"] ?? "";
    seen.push(key);
    const step = plan[key];
    if (step === undefined) return route.fulfill({ status: 599, body: `scriptByKey: no plan for key "${key}"` });
    return route.fulfill(answer(step));
  });
  return { count: (key: string) => seen.filter((k) => k === key).length };
}

// ---- watchRequests: what the browser SENT --------------------------------------------------------
//
//   const sent = watchRequests(page, "catalog.v1.CatalogService/QuotePrices");   // before page.goto
//   sent.sent("OPS", "SW-SKU")   // calls sent with X-API-Key OPS whose body contains "SW-SKU"
//
// Counts every call the browser sent to `rpc` (exact, as scriptByKey), parked by a hold, failed by
// failRequests or answered: it is the browser's own "request" event. key "" = no X-API-Key; `text`
// (case-sensitive) narrows to bodies containing it. Next to a hold it shows that the call a test is about is
// among the parked ones: sent(key, text) >= 1 and sent(key) === hold.count().
export function watchRequests(page: Page, rpc: string) {
  const sent: { key: string; body: string }[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith(`/${rpc}`)) sent.push({ key: r.headers()["x-api-key"] ?? "", body: r.postData() ?? "" });
  });
  return { sent: (key: string, text = "") => sent.filter((s) => s.key === key && s.body.includes(text)).length };
}
