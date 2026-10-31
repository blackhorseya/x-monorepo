// Performance baseline, one phase per run (tests/k6/bench.sh drives it N times; `task k6-bench`).
// Unlike orders.js / reservation.js this has no latency threshold to pass: the numbers are the
// product, the thresholds below only insist that every operation succeeded (a baseline of a
// failing run is not a baseline) and make k6 export the per-RPC sub-metrics.
//   k6 run tests/k6/bench.js -e PHASE=create|reserve -e BASE_URL=… -e RUN_ID=… [-e VUS=20] [-e RATE=100] [-e DURATION=30s] [-e SEED=1]
//
//   PHASE=create   closed model: VUS virtual users call CreateOrder back to back (no think time)
//                  for DURATION. Measures the write path's throughput and latency at saturation.
//   PHASE=reserve  open model: RATE orders per second for DURATION, each one CreateOrder and then
//                  polls GetReservation every POLL ms until RESERVED. Two latencies, named for
//                  what they span: order_to_reserved is from before CreateOrder is sent to RESERVED
//                  seen (the end-to-end number: CreateOrder + outbox wait + worker batch +
//                  handler); reserved_after_create starts when CreateOrder has returned (the part
//                  the worker owns). Both with whatever backlog RATE builds up in the outbox.
//
// Reproducible input: the SKU of iteration i comes from a PRNG seeded with SEED and i
// (exec.scenario.iterationInTest), never Math.random, so two runs with the same parameters ask for
// the same SKUs in the same order (with VUS=1 / one VU at a time; with more, the interleaving is
// the scheduler's). Throughput is computed by bench_report.py from bench_iter_start_ms /
// bench_create_end_ms (the first iteration's start to the last CreateOrder's return), not from
// k6's Counter rate, whose denominator is the whole test including setup().
//
// Writes are bounded: CreateOrder runs for DURATION at VUS (create) or exactly RATE×DURATION times
// (reserve), all under RUN_ID; bench.sh runs each in its own database and drops it afterwards.
// The scripts never print a response body or the URL (lib.js), so an API_KEY stays out of the
// output, the summary and the exported JSON.
import http from "k6/http";
import exec from "k6/execution";
import { sleep } from "k6";
import { Counter, Trend } from "k6/metrics";
import { rpc, ok, sku, setStock, upsertProduct, createOrder, customer, statusTag, systemTags, throwOnError } from "./lib.js";
import { RESERVATION_STATUSES } from "./codes.js";

// GetReservation is 404 until the worker has consumed the order: the poll's "not yet".
http.setResponseCallback(http.expectedStatuses(200, 404));

const PHASE = __ENV.PHASE || "create";
if (!["create", "reserve"].includes(PHASE)) {
  throw new Error(`PHASE=${PHASE}: want create or reserve`);
}
const SKUS = Number(__ENV.SKUS || 50);
const DURATION = __ENV.DURATION || "30s";
const POLL = Number(__ENV.POLL || 50);
const RESERVE_TIMEOUT = Number(__ENV.RESERVE_TIMEOUT || 30_000);

const SEED = Number(__ENV.SEED || 1);

// the scenario's own clock, epoch ms: min/max of these two are its first start and last CreateOrder end
const iterStart = new Trend("bench_iter_start_ms");
const createEnd = new Trend("bench_create_end_ms");
const orderToReserved = new Trend("order_to_reserved", true);
const reservedAfterCreate = new Trend("reserved_after_create", true);
const reservationTimeouts = new Counter("reservation_timeouts");
// orders that were created and then seen RESERVED: bench_report.py reconciles it with the CreateOrder
// successes (an iteration cut off by gracefulStop is in neither this nor reservation_timeouts)
const reservedOk = new Counter("reserved_ok");
const reservationWrong = new Counter("reservation_wrong_status");

const scenarios = {
  create: {
    executor: "constant-vus",
    vus: Number(__ENV.VUS || 20),
    duration: DURATION,
    exec: "create",
  },
  reserve: {
    executor: "constant-arrival-rate",
    rate: Number(__ENV.RATE || 100),
    timeUnit: "1s",
    duration: DURATION,
    preAllocatedVUs: Number(__ENV.PRE_VUS || 50),
    maxVUs: Number(__ENV.MAX_VUS || 400),
    exec: "reserve",
    // an iteration is a CreateOrder (up to rpc()'s 30s timeout) and then a wait of up to RESERVE_TIMEOUT:
    // the grace must cover both, or a slow tail is cut off silently (k6 exits 0 for that)
    gracefulStop: `${30_000 + RESERVE_TIMEOUT + 5000}ms`,
  },
};

export const options = {
  throw: throwOnError,
  systemTags,
  scenarios: { [PHASE]: scenarios[PHASE] },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    rpc_errors: ["count==0"],
    reservation_timeouts: ["count==0"],
    reservation_wrong_status: ["count==0"],
    // always true: makes k6 export how many rpc_errors were timeouts, so bench_report.py can tell an
    // overloaded machine (every error a timeout) from a real failure (#288)
    "rpc_errors{code:request.timeout}": ["count>=0"],
    "rpc_errors{code:transport.timeout}": ["count>=0"],
    // always true: makes k6 export these sub-metrics, and fails a run that did nothing
    "rpc_duration{rpc:CreateOrder}": ["max>=0"],
    "rpc_ok{rpc:CreateOrder}": ["count>0"],
    ...(PHASE === "reserve" ? { order_to_reserved: ["max>=0"], reserved_after_create: ["max>=0"] } : {}),
  },
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
};

export function setup() {
  for (let i = 0; i < SKUS; i++) {
    upsertProduct(i, 1000); // first: SetStock refuses a SKU the catalog does not have, and CreateOrder prices from it
    setStock(i, 10_000_000);
  }
}

// mulberry32: a small seeded PRNG, one stream per iteration
function mulberry32(a) {
  return function () {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function order() {
  const rand = mulberry32((SEED + Math.imul(exec.scenario.iterationInTest, 0x9e3779b1)) >>> 0);
  return {
    items: [
      {
        sku: sku(Math.floor(rand() * SKUS)),
        quantity: 1,
      },
    ],
  };
}

export function create() {
  iterStart.add(Date.now());
  ok(createOrder(order(), customer(`bench-${__VU}`)), "CreateOrder");
  createEnd.add(Date.now());
}

export function reserve() {
  const sent = Date.now();
  iterStart.add(sent);
  const created = ok(createOrder(order(), customer(`bench-${__VU}`)), "CreateOrder");
  const start = Date.now(); // CreateOrder has returned
  createEnd.add(start);
  if (!created) return;
  for (;;) {
    const res = rpc(
      "inventory.v1.InventoryService/GetReservation",
      { order_id: created.order.id },
      { tags: { poll: "true" }, probe: true }
    );
    if (res.status === 200) {
      const status = res.json("reservation.status");
      if (status === "RESERVATION_STATUS_RESERVED") {
        const now = Date.now();
        orderToReserved.add(now - sent);
        reservedAfterCreate.add(now - start);
        reservedOk.add(1);
        ok(res, "GetReservation");
      } else {
        reservationWrong.add(1, { status: statusTag(status, RESERVATION_STATUSES) });
      }
      return;
    }
    if (res.status !== 404) {
      ok(res, "GetReservation"); // counts the error with its code
      return;
    }
    if (Date.now() - start > RESERVE_TIMEOUT) {
      reservationTimeouts.add(1);
      return;
    }
    sleep(POLL / 1000);
  }
}
