import { expect, test, type Page } from "@playwright/test";
import { settle, useKey } from "./auth/helpers";
import { failRequests, holdRequests, scriptByKey, watchRequests, type Reply } from "./fail-requests";
import { LIST_POISON, PAGE_BODY, PROBE_BODY } from "./probe";

// #483. Switching credential must re-ask every query the page is showing, with the NEW credential, and
// must not show what was read for the old one. Each credential gets its own scripted answer (e2e/fail-requests.ts)
// and the answer for the old one is parked by `holdRequests` until the test releases it after the switch,
// so the interleavings are made by the test and not by timing.
//
// The page is /poison. Its list (ListPoison, an empty request, polled every 5 s) is what is shown, and the rows
// are what only a successful answer renders: a scripted parked message per credential, named after it. The admin
// probe (ListPoison, limit 1: e2e/probe.ts) asks the same RPC about the same credential: it opens the page
// (AdminGate), and a scripted answer is per credential, so the probe and the list agree. Holds and faults
// that are meant for one of the two name its body (PROBE_BODY / PAGE_BODY).

const messageOf = (bc: string) => ({ boundedContext: bc, offset: "1", uuid: `u-${bc}`, topic: "t.v1", handler: "h", reason: `r-${bc}` });
const listOf = (...bcs: string[]): Reply => ({ ok: { messages: bcs.map(messageOf) } });
// The list shows the bounded context of each parked message, which these tests make equal to the credential's name.
const rowOf = (page: Page, bc: string) => page.getByRole("row").filter({ hasText: bc });
const BC_A = "bc-a";
const BC_B = "bc-b";
const BC_OPS = "bc-ops";

const METHOD = "ListPoison";
const PAGE = (key: string | null) => ({ method: METHOD, key, bodyIncludes: PAGE_BODY });
const PROBE = (key: string | null) => ({ method: METHOD, key, bodyIncludes: PROBE_BODY });
const NOT_ADMIN: Reply = { connect: "unauthenticated" };

// A reply is "late" when the credential it was asked with is no longer the one in use. The response is awaited
// in the browser (not slept for) and the page is given its frames to react before the caller asserts that
// nothing of it is shown: a negative assertion made after a provable delivery, not after a guessed delay.
async function deliverLate(page: Page, key: string, release: () => void) {
  const arrived = page.waitForResponse((r) => r.url().endsWith(LIST_POISON) && (r.request().headers()["x-api-key"] ?? "") === key);
  release();
  await arrived;
  await settle(page);
}

test("the anonymous probe is still in flight when the key is set; the page shows the key's rows, and the late 401 changes nothing", async ({ page }) => {
  const list = await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, OPS: listOf(BC_OPS) });
  const held = await holdRequests(page, PROBE(null));
  await page.goto("/poison");
  await held.arrived(); // the anonymous probe is on the wire, unanswered

  await useKey(page, "OPS");
  await expect(rowOf(page, BC_OPS)).toBeVisible();
  await deliverLate(page, "", () => held.release()); // the old answer (401) arrives after the switch

  await expect(rowOf(page, BC_OPS)).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByText("Admin role required")).toHaveCount(0);
  expect(list.count("OPS"), "ListPoison sent with the new key").toBeGreaterThanOrEqual(1);
});

test("a list reply for the old credential that arrives after the switch is not shown, and the new credential is asked", async ({ page }) => {
  // The anonymous probe says "not admin", A's list call is held, and the page switches again to B: a query
  // with no data and a call in flight is the case where a second request used to be folded into the first.
  // B's rows must come from a call B made, and A's late rows must never be shown.
  const list = await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const heldA = await holdRequests(page, PAGE("A"));
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await heldA.arrived(); // A's list call is in flight

  await useKey(page, "B");
  await expect(rowOf(page, BC_B)).toBeVisible();
  await deliverLate(page, "A", () => heldA.release()); // A's rows arrive late

  await expect(rowOf(page, BC_A)).toHaveCount(0);
  await expect(rowOf(page, BC_B)).toBeVisible();
  expect(list.count("B"), "ListPoison sent with B").toBeGreaterThanOrEqual(1);
});

test("rows read for the old credential do not stay on screen while the new credential's answer is awaited", async ({ page }) => {
  await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const heldB = await holdRequests(page, PAGE("B"));
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await expect(rowOf(page, BC_A)).toBeVisible();

  await useKey(page, "B");
  await heldB.arrived(); // B's list call is in flight
  await expect(rowOf(page, BC_A), "A's rows while B's answer is pending").toHaveCount(0);

  heldB.release();
  await expect(rowOf(page, BC_B)).toBeVisible();
});

test("A's rows arrive late while B's call is still pending; they are not shown, B's rows are", async ({ page }) => {
  await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const heldA = await holdRequests(page, PAGE("A"));
  const heldB = await holdRequests(page, PAGE("B"));
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await heldA.arrived(); // A's list call is in flight
  await useKey(page, "B");
  await heldB.arrived(); // positive control: B's call is parked, so the empty page below is "B pending", not "nothing asked"

  await deliverLate(page, "A", () => heldA.release());
  await expect(rowOf(page, BC_A), "A's rows after A's late answer, B still pending").toHaveCount(0);

  heldB.release();
  await expect(rowOf(page, BC_B)).toBeVisible();
  await expect(rowOf(page, BC_A)).toHaveCount(0);
});

test("A's list call fails late (PermissionDenied) after the switch: the alert is not shown for B", async ({ page }) => {
  // The late FAILURE of the same interleaving. Handlers run newest first: the hold is registered after the fault,
  // so it parks A's list call and, on release, hands it on to the fault, which answers it 403.
  await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const fault = await failRequests(page, { ...PAGE("A"), mode: "permission_denied", times: Infinity });
  const heldA = await holdRequests(page, PAGE("A"));
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await heldA.arrived(); // A's list call is parked, not yet failed
  expect(fault.failed(), "the fault has not been served yet").toBe(0);

  await useKey(page, "B");
  await expect(rowOf(page, BC_B)).toBeVisible();
  await deliverLate(page, "A", () => heldA.release()); // A's answer, a 403, arrives after the switch

  expect(fault.failed(), "A's 403 was really served").toBe(1);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(rowOf(page, BC_B)).toBeVisible();
});

// ---- retry x credential (#483; needs the bounded retry of src/transient.ts) ---------------------------------------
//
// A's first list call is refused (503), so the query is in its backoff and then its retry. The page switches to B
// while A's retry is on the wire / waiting: the retry belongs to A. Handlers run newest first: the fault takes A's
// first call, the hold parks the call after it (the retry), the script answers whatever reaches it.

test("a retry in flight for the old credential: B is asked at once, and the retry's late rows are not shown", async ({ page }) => {
  const list = await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const heldA = await holdRequests(page, PAGE("A"));
  const fault = await failRequests(page, PAGE("A")); // A's first list call: 503
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await heldA.arrived(); // the refused call was retried: the retry is on the wire, unanswered
  expect([fault.failed(), heldA.count()], "A: one refused call, one parked retry").toEqual([1, 1]);

  await useKey(page, "B");
  await expect(rowOf(page, BC_B), "B's rows while A's retry is still parked").toBeVisible();
  await deliverLate(page, "A", () => heldA.release()); // A's retry answers after the switch

  await expect(rowOf(page, BC_A)).toHaveCount(0);
  await expect(rowOf(page, BC_B)).toBeVisible();
  expect(list.count("B"), "ListPoison sent with B").toBeGreaterThanOrEqual(1);
});

test("a retry waiting out its backoff for the old credential is not sent after the switch", async ({ page }) => {
  await scriptByKey(page, LIST_POISON, { "": NOT_ADMIN, A: listOf(BC_A), B: listOf(BC_B) });
  const seen = watchRequests(page, LIST_POISON);
  const fault = await failRequests(page, { ...PAGE("A"), times: Infinity }); // every A list call: 503
  await page.goto("/poison");
  await expect(page.getByText("Admin role required")).toBeVisible();
  await useKey(page, "A");
  await expect.poll(() => fault.failed()).toBeGreaterThanOrEqual(1); // A's first list call was refused; its retry is waiting out the 500ms backoff
  await useKey(page, "B");
  const sentA = seen.sent("A", PAGE_BODY); // taken at the switch: the calls A still had coming are the ones after this
  await expect(rowOf(page, BC_B)).toBeVisible();

  // past the whole retry budget (3.5s): the calls A was still going to make never come, and nothing of A is shown
  await page.waitForTimeout(4_000);
  expect([seen.sent("A", PAGE_BODY), fault.failed()], "A's list calls after the switch").toEqual([sentA, sentA]);
  await expect(rowOf(page, BC_B)).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});
