import type { Page, Request, Route } from "@playwright/test";

// The console's admin probe (src/queries.ts adminProbeQuery) is admin.v1.AdminService/ListPoison with limit 1.
// The Poison page's own list asks the same RPC with an empty request ({}), polled every 5 s, so a stub meant
// for one of the two has to tell them apart by the request body. These are the two bodies, in one place.
export const LIST_POISON = "admin.v1.AdminService/ListPoison";
/** In the body of the probe (and never in the page's own list). */
export const PROBE_BODY = '"limit":1';
/** The page's own list: an empty request. `{}` is not a substring of the probe's body. */
export const PAGE_BODY = "{}";

export const isProbe = (r: Request) => r.url().endsWith(`/${LIST_POISON}`) && (r.postData() ?? "").includes(PROBE_BODY);

/**
 * Handle the probe's requests; every other request goes on to the next handler (route.fallback).
 * Register it before page.goto.
 */
export function routeProbe(page: Page, handler: (route: Route) => Promise<void> | void) {
  return page.route(`**/${LIST_POISON}`, (route) => (isProbe(route.request()) ? handler(route) : route.fallback()));
}

/** Resolves with the next probe response that satisfies `want` (default: any). */
export const probeResponse = (page: Page, want: (r: { status: number; key: string }) => boolean = () => true) =>
  page.waitForResponse((r) => isProbe(r.request()) && want({ status: r.status(), key: r.request().headers()["x-api-key"] ?? "" }));

/**
 * What an admin key's probe is answered with by the e2e rpc that has no database (the `auth` project's, and `chromium`'s
 * without E2E_DATABASE_URL): the role check passes, then the database check fails: FAILED_PRECONDITION =
 * HTTP 400 (x-error-code admin.no_database). The console reads that as "admin" (src/queries.ts).
 */
export const ADMIN_NO_DB_STATUS = 400;
