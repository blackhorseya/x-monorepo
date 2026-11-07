import type { Page, Route } from "@playwright/test";

// Minimal request-level injection for the specs of #483. The rpc is scripted (page.route), not real:
// a test says what each CREDENTIAL gets from one RPC, and may hold an answer back until the test
// lets it go, which is how "the reply arrives after the credential changed" is made deterministic.
// (A shared helper may land in e2e/ from another PR; whichever merges second switches to that one.)

// What a scripted call is answered with. `connect` is a Connect error answer (the HTTP status follows the code).
export type Reply = { ok: object } | { connect: "unauthenticated" | "permission_denied" | "unavailable" } | { http: number };
// A step: a Reply, "hold" (keep the call until release()), "pass" (the real rpc answers) or "reset" (the connection resets).
export type Step = Reply | "hold" | "pass" | "reset";

const answer = (r: Reply) => {
  if ("ok" in r) return { status: 200, contentType: "application/json", body: JSON.stringify(r.ok) };
  if ("http" in r) return { status: r.http, contentType: "text/plain", body: "injected" };
  const status = { unauthenticated: 401, permission_denied: 403, unavailable: 503 }[r.connect];
  return { status, contentType: "application/json", body: JSON.stringify({ code: r.connect, message: `injected: ${r.connect}` }) };
};

// The credential a request carries: the X-API-Key value ("" = anonymous).
export const keyOf = (route: Route) => route.request().headers()["x-api-key"] ?? "";

// plan[key]: what the calls made with that key get: one Step for all of them, or a list consumed in order (the
// last one repeats). plan["*"] is every key not named. A key with no plan is a test bug and fails the call
// loudly (HTTP 599) instead of passing to the rpc. `only`: just the calls whose request body contains this text
// are scripted and counted, the others go to the real rpc (the order form quotes its default SKU first; the
// call a test is about is the one that names the test's own SKU).
export async function scriptByKey(page: Page, rpc: string, plan: Record<string, Step | Step[]>, { only }: { only?: string } = {}) {
  const seen: string[] = []; // the key of every scripted call, in the order the browser sent them
  const held = new Map<string, Route[]>();
  const heldBodies = (key: string) => (held.get(key) ?? []).map((r) => r.request().postData() ?? "");
  await page.route(`**/${rpc}`, async (route) => {
    if (only !== undefined && !(route.request().postData() ?? "").includes(only)) return route.continue();
    const key = keyOf(route);
    const n = seen.filter((k) => k === key).length; // how many calls this key made before this one
    seen.push(key);
    const entry = plan[key] ?? plan["*"];
    if (entry === undefined) return route.fulfill({ status: 599, body: `scriptByKey: no plan for key "${key}"` });
    const step = Array.isArray(entry) ? entry[Math.min(n, entry.length - 1)] : entry;
    if (step === "hold") return void held.set(key, [...(held.get(key) ?? []), route]);
    if (step === "pass") return route.continue();
    if (step === "reset") return route.abort("connectionreset");
    return route.fulfill(answer(step));
  });
  return {
    seen: () => [...seen],
    count: (key: string) => seen.filter((k) => k === key).length,
    total: () => seen.length,
    // Is a call held for `key` (whose request body contains `text`, when given)?
    isHeld: (key: string, text = "") => heldBodies(key).some((b) => b.includes(text)),
    // Answer every call held for `key`, now.
    release: async (key: string, reply: Reply) => {
      const routes = held.get(key) ?? [];
      held.delete(key);
      await Promise.all(routes.map((r) => r.fulfill(answer(reply))));
    },
  };
}
