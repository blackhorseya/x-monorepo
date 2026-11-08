import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  InfiniteQueryObserver,
  notifyManager,
  QueryClient,
  QueryObserver,
  type QueryObserverOptions,
} from "@tanstack/react-query";
import { ReservationStatus } from "@repo/proto/inventory/v1/entity_pb";
import { OrderStatus } from "@repo/proto/ordering/v1/entity_pb";
import * as queries from "./queries.ts";
import { queryClient } from "./queryClient.ts";
import { MAX_RETRY_DELAY_MS, retryDelay, shouldRetry } from "./transient.ts";
import { OK, server } from "./testing/client.stub.ts";

//   node --test --experimental-strip-types --import ./src/testing/register.mjs src/queries.retry.test.ts   (task web-test)
//
// What the retry policy does to each query AFTER react-query merged queryClient.ts's defaults with the
// query's own options. transient.test.ts proves the classifier; this proves nobody overrode it (#483 review:
// `retry: false` on whoAmIQuery and `retry: 3` on adminProbeQuery passed every test that looked at the
// classifier alone). The real QueryClient defaults (queryClient.getDefaultOptions(), not a copy), the real
// options exported by queries.ts and their real queryFn run; only the network (src/testing/client.stub.ts,
// global fetch for /api/version) is scripted, and the clock is mocked, so the 500 ms backoff costs nothing.
//
// The list of queries is whatever queries.ts exports: a new `*Query` export that is not in CASES below fails
// "every query is in the matrix", an export that is neither a query nor in NOT_QUERIES fails the same way.

// Arguments for each exported query factory; null = the export is the options object itself.
const CASES: Record<string, unknown[] | null> = {
  versionQuery: null,
  ordersQuery: ["customer-1", OrderStatus.PENDING],
  orderQuery: ["order-1", 0],
  historyQuery: ["order-1", 0, undefined],
  reservationQuery: ["order-1"],
  paymentQuery: ["order-1", 0],
  stocksQuery: ["SKU-", false],
  quoteQuery: [["SKU-1"]],
  productsQuery: ["SKU-"],
  productStatusQuery: [["SKU-1"]],
  reservationsQuery: [ReservationStatus.UNSPECIFIED],
  poisonQuery: null,
  poisonDetailQuery: ["orders", 1n],
  adminProbeQuery: [0],
  whoAmIQuery: [0],
  accountsQuery: null,
  accountQuery: ["account-1"],
  accountIdentitiesQuery: ["account-1", 0, true],
  apiKeysQuery: ["account-1"],
};

// Every other export of queries.ts: helpers and constants, not queries.
const NOT_QUERIES = new Set([
  "keys",
  "POLL_MS",
  "SLOW_POLL_MS",
  "PRODUCT_STATUS_MAX",
  "refreshFirstPage",
  "refreshOrders",
  "refreshStocks",
  "refreshReservations",
  "orderInterval",
  "historyInterval",
  "reservationInterval",
  "reservationPending",
]);

const exported = Object.keys(queries);
const queryNames = exported.filter((n) => n.endsWith("Query"));

describe("every query is in the matrix", () => {
  it("each export is a listed query or a listed helper", () => {
    const unknown = exported.filter((n) => !n.endsWith("Query") && !NOT_QUERIES.has(n));
    assert.deepEqual(unknown, [], "queries.ts exports names this test does not classify: add them to NOT_QUERIES or CASES");
  });
  it("each *Query export has a row in CASES", () => {
    const missing = queryNames.filter((n) => !(n in CASES));
    assert.deepEqual(missing, [], "a query without a row in the matrix: its retry policy is untested");
  });
  it("each row of CASES is still exported", () => {
    const stale = Object.keys(CASES).filter((n) => !queryNames.includes(n));
    assert.deepEqual(stale, []);
  });
  it("the matrix is not empty", () => assert.ok(queryNames.length >= 10, `${queryNames.length} queries`));
});

describe("the app's defaults are the shared policy", () => {
  const d = queryClient.getDefaultOptions();
  it("queries use the classifier and its backoff", () => {
    assert.equal(d.queries?.retry, shouldRetry);
    assert.equal(d.queries?.retryDelay, retryDelay);
  });
  it("mutations are never retried", () => assert.equal(d.mutations?.retry, 0));
});

type Opts = QueryObserverOptions<any, any, any, any, any>;

function optionsOf(name: string): Opts {
  const entry = (queries as Record<string, unknown>)[name];
  const args = CASES[name];
  assert.notEqual(args, undefined, `${name} has no row in CASES`);
  const opts = typeof entry === "function" ? (entry as (...a: unknown[]) => Opts)(...(args ?? [])) : (entry as Opts);
  assert.ok(opts?.queryFn, `${name} did not give options with a queryFn`);
  return opts;
}

const settle = async () => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

// One query on a fresh client built from the app's own defaults, watched the way useQuery watches it.
async function withObserver<T>(name: string, body: (o: { result: () => { status: string; data: unknown } }) => Promise<T>) {
  const qc = new QueryClient({ defaultOptions: queryClient.getDefaultOptions() });
  const opts = optionsOf(name);
  const observer = "initialPageParam" in opts ? new InfiniteQueryObserver(qc, opts as any) : new QueryObserver(qc, opts);
  const unsubscribe = observer.subscribe(() => {});
  try {
    return await body({ result: () => observer.getCurrentResult() });
  } finally {
    unsubscribe();
    qc.clear();
  }
}

const fetched: string[] = [];
const realFetch = globalThis.fetch;
type Scenario = { kind: "transient" } | { kind: "answer"; code: Code };

// The server is scripted per scenario. transient: the first request fails the way a refusing server (503 ->
// Unavailable) or a reset connection fails, the next ones succeed. answer: every request is a domain answer.
function script(s: Scenario) {
  server.reset();
  fetched.length = 0;
  const total = () => server.calls.length + fetched.length;
  server.answer = () => {
    if (s.kind === "transient") return total() === 1 ? { fail: new ConnectError("scripted outage", Code.Unavailable) } : { ok: OK };
    return { fail: new ConnectError("scripted answer", s.code) };
  };
  // GET /api/version is plain fetch: its transient failure is the network error the classifier knows; an
  // answer is any non-2xx (the query throws a plain Error for it).
  globalThis.fetch = (async () => {
    fetched.push("GET /api/version");
    if (s.kind === "transient" && total() === 1) throw new TypeError("Failed to fetch");
    if (s.kind === "answer") return new Response("no", { status: 400 });
    return Response.json({ version: "v", commit: "c", date: "d", go: "g" });
  }) as typeof fetch;
  return total;
}

beforeEach(() => {
  notifyManager.setScheduler((cb) => queueMicrotask(cb));
  mock.timers.enable({ apis: ["setTimeout"] });
});
afterEach(() => {
  mock.timers.reset();
  globalThis.fetch = realFetch;
  server.reset();
});

describe("a transient failure is retried by the shared policy", () => {
  for (const name of queryNames) {
    it(`${name}: Unavailable once -> asked again after the shared delay (${retryDelay(0)} ms), then it succeeds`, async () => {
      const total = script({ kind: "transient" });
      await withObserver(name, async ({ result }) => {
        await settle();
        assert.equal(total(), 1, "the first request was sent");
        mock.timers.tick(retryDelay(0) - 1);
        await settle();
        assert.equal(total(), 1, "still waiting: the shared backoff, not a shorter one");
        mock.timers.tick(1);
        await settle();
        assert.equal(total(), 2, "asked again exactly once");
        assert.equal(result().status, "success");
      });
    });
  }
});

describe("an answer is asked once", () => {
  for (const name of queryNames) {
    for (const code of [Code.InvalidArgument, Code.PermissionDenied]) {
      it(`${name}: ${Code[code]} -> one request, never asked again`, async () => {
        const total = script({ kind: "answer", code });
        await withObserver(name, async ({ result }) => {
          await settle();
          assert.equal(total(), 1, "the first request was sent");
          mock.timers.tick(MAX_RETRY_DELAY_MS * 4);
          await settle();
          assert.equal(total(), 1, "no second request after the whole backoff");
          // Not "error": adminProbeQuery turns PermissionDenied into the answer `false` (success), either way settled.
          assert.notEqual(result().status, "pending", "and it settled (an answer is shown at once)");
        });
      });
    }
  }
});
