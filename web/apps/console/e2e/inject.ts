import type { Page, Route } from "@playwright/test";

// Scripted rpc for the specs of #483: a test says what each CREDENTIAL gets from one RPC. Holding an answer
// back until after the credential changed is not done here: that is `holdRequests` (e2e/fail-requests.ts),
// registered AFTER scriptByKey so it sees the call first and, on release, hands it on to this script.

// What a scripted call is answered with. `connect` is a Connect error answer (the HTTP status follows the code).
export type Reply = { ok: object } | { connect: "unauthenticated" | "permission_denied" | "unavailable" } | { http: number };

const answer = (r: Reply) => {
  if ("ok" in r) return { status: 200, contentType: "application/json", body: JSON.stringify(r.ok) };
  if ("http" in r) return { status: r.http, contentType: "text/plain", body: "injected" };
  const status = { unauthenticated: 401, permission_denied: 403, unavailable: 503 }[r.connect];
  return { status, contentType: "application/json", body: JSON.stringify({ code: r.connect, message: `injected: ${r.connect}` }) };
};

// The credential a request carries: the X-API-Key value ("" = anonymous).
export const keyOf = (route: Route) => route.request().headers()["x-api-key"] ?? "";

// plan[key]: the Reply for a call made with that key. A key not in the plan is a test bug and fails the
// call loudly (HTTP 599) instead of passing to the rpc.
export async function scriptByKey(page: Page, rpc: string, plan: Record<string, Reply>) {
  const seen: string[] = []; // the key of every call that reached this script, in order
  await page.route(`**/${rpc}`, async (route) => {
    const key = keyOf(route);
    seen.push(key);
    const step = plan[key];
    if (step === undefined) return route.fulfill({ status: 599, body: `scriptByKey: no plan for key "${key}"` });
    return route.fulfill(answer(step));
  });
  return { count: (key: string) => seen.filter((k) => k === key).length };
}

// Every call the browser SENT to `rpc` (parked by a hold or not), by key; `text` narrows to calls whose body
// contains it. With it a test can show that the call it is about is among the ones a hold parked:
// sent(key, text) >= 1 and sent(key) === hold.count().
export function watchRequests(page: Page, rpc: string) {
  const sent: { key: string; body: string }[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith(`/${rpc}`)) sent.push({ key: r.headers()["x-api-key"] ?? "", body: r.postData() ?? "" });
  });
  return { sent: (key: string, text = "") => sent.filter((s) => s.key === key && s.body.includes(text)).length };
}
