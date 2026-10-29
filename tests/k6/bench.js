// Performance baseline, one phase per run (tests/k6/bench.sh drives it N times; `task k6-bench`).
// Unlike orders.js / reservation.js this has no latency threshold to pass: the numbers are the
// product, the thresholds below only insist that every operation succeeded (a baseline of a
// failing run is not a baseline) and make k6 export the per-RPC sub-metrics.
//   k6 run tests/k6/bench.js -e PHASE=create|reserve -e BASE_URL=… -e RUN_ID=… [-e VUS=20] [-e RATE=100] [-e DURATION=30s]
//
//   PHASE=create   closed model: VUS virtual users call CreateOrder back to back (no think time)
//                  for DURATION. Measures the write path's throughput and latency at saturation.
//   PHASE=reserve  open model: RATE orders per second for DURATION, each one CreateOrder and then
//                  polls GetReservation every POLL ms until RESERVED. reservation_latency is
//                  CreateOrder-returned → RESERVED seen, i.e. outbox wait + worker batch + handler,
//                  with whatever backlog RATE builds up in the outbox (the worker's batching
//                  shows in how that latency behaves under a backlog, not at idle).
//
// Writes are bounded: CreateOrder runs for DURATION at VUS (create) or exactly RATE×DURATION times
// (reserve), all under RUN_ID; bench.sh runs each in its own database and drops it afterwards.
// The scripts never print a response body or the URL (lib.js), so an API_KEY stays out of the
// output, the summary and the exported JSON.
import http from "k6/http";
import { sleep } from "k6";
import { Counter, Trend } from "k6/metrics";
import { rpc, ok, sku, setStock, createOrder, customer, statusTag, systemTags, throwOnError } from "./lib.js";
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

const reservationLatency = new Trend("reservation_latency", true);
const reservationTimeouts = new Counter("reservation_timeouts");
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
    gracefulStop: `${RESERVE_TIMEOUT + 5000}ms`,
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
    // always true: makes k6 export these sub-metrics, and fails a run that did nothing
    "rpc_duration{rpc:CreateOrder}": ["max>=0"],
    "rpc_ok{rpc:CreateOrder}": ["count>0"],
    ...(PHASE === "reserve" ? { reservation_latency: ["max>=0"] } : {}),
  },
  summaryTrendStats: ["avg", "min", "med", "p(90)", "p(95)", "p(99)", "max"],
};

export function setup() {
  for (let i = 0; i < SKUS; i++) {
    setStock(i, 10_000_000);
  }
}

function order() {
  return {
    items: [
      {
        sku: sku(Math.floor(Math.random() * SKUS)),
        quantity: 1,
        unit_price: { amount_minor: 1000, currency: "USD" },
      },
    ],
  };
}

export function create() {
  ok(createOrder(order(), customer(`bench-${__VU}`)), "CreateOrder");
}

export function reserve() {
  const created = ok(createOrder(order(), customer(`bench-${__VU}`)), "CreateOrder");
  if (!created) return;
  const start = Date.now();
  for (;;) {
    const res = rpc(
      "inventory.v1.InventoryService/GetReservation",
      { order_id: created.order.id },
      { tags: { poll: "true" }, probe: true }
    );
    if (res.status === 200) {
      const status = res.json("reservation.status");
      if (status === "RESERVATION_STATUS_RESERVED") {
        reservationLatency.add(Date.now() - start);
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
