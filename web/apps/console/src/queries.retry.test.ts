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
import * as queries from "./queries.ts";
import { queryClient } from "./queryClient.ts";
import { MAX_RETRY_DELAY_MS, retryDelay, shouldRetry } from "./transient.ts";
import { OK, server } from "./testing/client.stub.ts";

//   node --test --experimental-strip-types --import ./src/testing/register.mjs src/queries.retry.test.ts   (task web-test)
//
// What the retry policy does to each query AFTER react-query merged queryClient.ts's defaults with the
// query's own options. transient.test.ts proves the classifier; this proves nobody overrode it (#483 review:
// `retry: false` on one query and `retry: 3` on adminProbeQuery passed every test that looked at the
// classifier alone). The real QueryClient defaults (queryClient.getDefaultOptions(), not a copy), the real
// options exported by queries.ts and their real queryFn run; only the network (src/testing/client.stub.ts,
// global fetch for /api/version) is scripted, and the clock is mocked, so the 500 ms backoff costs nothing.
//
// The list of queries is whatever queries.ts exports: a new `*Query` export that is not in CASES below fails
// "every query is in the matrix", an export that is neither a query nor in NOT_QUERIES fails the same way.

// Arguments for each exported query factory; null = the export is the options object itself.
const CASES: Record<string, unknown[] | null> = {
  versionQuery: null,
  productsQuery: ["SKU-"],
  poisonQuery: null,
  poisonDetailQuery: ["schema", 1n],
  adminProbeQuery: [0],
  accountsQuery: null,
  accountQuery: ["account-1"],
  accountIdentitiesQuery: ["account-1", 0, true],
  apiKeysQuery: ["account-1"],
};

// Every other export of queries.ts: helpers and constants, not queries.
const NOT_QUERIES = new Set([
  "keys",
]);

// Queries whose queryFn turns a Connect answer into data: the cell asserts the data too (a regression that rethrows
// it would still be "asked once", but the user would see an error instead of "not admin").
const ANSWERED_AS_DATA: Record<string, Partial<Record<Code, { value: unknown }>>> = {
  adminProbeQuery: { [Code.PermissionDenied]: { value: false } },
};

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
  it("the matrix is not empty", () => assert.ok(queryNames.length >= 5, `${queryNames.length} queries`));
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
// transient: the first request fails once. For GET /api/version (plain fetch) that is an HTTP status by default
// (503) or a network error (via: "network"); `http` = a status that is an answer or transient, every request.
type Scenario =
  | { kind: "transient"; via?: "network" }
  | { kind: "answer"; code: Code }
  | { kind: "http"; status: number };

// The server is scripted per scenario. transient: the first request fails the way a refusing server (503 ->
// Unavailable) or a reset connection fails, the next ones succeed. answer: every request is a domain answer.
function script(s: Scenario) {
  server.reset();
  fetched.length = 0;
  const total = () => server.calls.length + fetched.length;
  server.answer = () => {
    if (s.kind === "http") return { ok: OK };
    if (s.kind === "transient") return total() === 1 ? { fail: new ConnectError("scripted outage", Code.Unavailable) } : { ok: OK };
    return { fail: new ConnectError("scripted answer", s.code) };
  };
  // GET /api/version is plain fetch: the first request of a transient scenario is a 503 (or a network error),
  // an answer is a 400, `http` answers every request with its status.
  globalThis.fetch = (async () => {
    fetched.push("GET /api/version");
    if (s.kind === "http") return new Response("x", { status: s.status });
    if (s.kind === "transient" && total() === 1) {
      if (s.via === "network") throw new TypeError("Failed to fetch");
      return new Response("unavailable", { status: 503 });
    }
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
          assert.notEqual(result().status, "pending", "and it settled (an answer is shown at once)");
          // Some queries turn an answer into data in their own queryFn (adminProbeQuery: PermissionDenied is the
          // answer `false`, not an error): that is the contract of the cell, so it is asserted, not just settled.
          const asData = ANSWERED_AS_DATA[name]?.[code];
          if (asData !== undefined) {
            assert.equal(result().status, "success", "the answer is cached as data, not shown as an error");
            assert.equal(result().data, asData.value, "and it is the query's answer");
          }
        });
      });
    }
  }
});

// GET /api/version is plain fetch, not an RPC: its HTTP status has to reach the shared policy as what it is.
describe("versionQuery over HTTP", () => {
  for (const status of [429, 502, 503, 504]) {
    it(`HTTP ${status} -> transient: asked again after the shared delay, then it succeeds`, async () => {
      const total = script({ kind: "transient" });
      globalThis.fetch = (async () => {
        fetched.push("GET /api/version");
        return total() === 1 ? new Response("x", { status }) : Response.json({ version: "v", commit: "c", date: "d", go: "g" });
      }) as typeof fetch;
      await withObserver("versionQuery", async ({ result }) => {
        await settle();
        assert.equal(total(), 1, "the first request was sent");
        mock.timers.tick(retryDelay(0));
        await settle();
        assert.equal(total(), 2, "asked again");
        assert.equal(result().status, "success");
      });
    });
  }
  it("a network error is transient too", async () => {
    const total = script({ kind: "transient", via: "network" });
    await withObserver("versionQuery", async ({ result }) => {
      await settle();
      assert.equal(total(), 1);
      mock.timers.tick(retryDelay(0));
      await settle();
      assert.equal(total(), 2, "asked again");
      assert.equal(result().status, "success");
    });
  });
  for (const status of [400, 404, 500]) {
    it(`HTTP ${status} -> an answer: one request, never asked again`, async () => {
      const total = script({ kind: "http", status });
      await withObserver("versionQuery", async ({ result }) => {
        await settle();
        assert.equal(total(), 1, "the first request was sent");
        mock.timers.tick(MAX_RETRY_DELAY_MS * 4);
        await settle();
        assert.equal(total(), 1, "no second request after the whole backoff");
        assert.equal(result().status, "error");
      });
    });
  }
});
