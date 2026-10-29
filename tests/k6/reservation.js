// End to end through the worker: CreateOrder → ordering outbox → worker → inventory
// reserves → GetReservation says RESERVED. Measures the one number orders.js leaves
// out — how long an order waits for its reservation — with a worker running next
// to the server (smoke.sh starts one; against task run, start `bin/worker` yourself).
//   k6 run tests/k6/reservation.js -e BASE_URL=http://127.0.0.1:8080 [-e VUS=5] [-e DURATION=30s]
// Each VU: CreateOrder (one line, qty 1, stock is plentiful) then polls
// GetReservation every POLL ms until RESERVED or RESERVE_TIMEOUT ms pass, then GetOrder
// until CONFIRMED (ordering consumed StockReserved; order_not_confirmed counts the misses).
// reservation_latency is CreateOrder-returned → RESERVED seen; the poll interval
// bounds its resolution. Thresholds: p95 under RESERVE_P95 (ms, default 3000:
// loose on purpose, for a shared CI runner). The outbox is polled: after an empty poll
// the worker waits up to worker.poll_interval (default 200ms) before it sees the order
// (laptop, 10 VU: p50 ~0.2s / p95 ~0.25s,
// against p50 ~0.9s / p95 ~1.1s when it was the library's 1s); the rest is the handler
// + SetStock contention. No timeouts, no REJECTED (stock is 1e6).
import http from "k6/http";
import { sleep } from "k6";
import { Counter, Trend } from "k6/metrics";
import { rpc, ok, sku, setStock, createOrder, customer } from "./lib.js";

// GetReservation is 404 (not_found) until the worker has consumed the order;
// that is the poll loop's "not yet", not a failed request.
http.setResponseCallback(http.expectedStatuses(200, 404));

const SKUS = Number(__ENV.SKUS || 20);
const POLL = Number(__ENV.POLL || 100);
const RESERVE_TIMEOUT = Number(__ENV.RESERVE_TIMEOUT || 30_000);
const RESERVE_P95 = Number(__ENV.RESERVE_P95 || 3000);

const reservationLatency = new Trend("reservation_latency", true);
const reservationTimeouts = new Counter("reservation_timeouts");
const reservationWrong = new Counter("reservation_wrong_status");
const orderNotConfirmed = new Counter("order_not_confirmed");
const reservationNotCommitted = new Counter("reservation_not_committed");

export const options = {
  scenarios: {
    reservation: {
      executor: "constant-vus",
      vus: Number(__ENV.VUS || 5),
      duration: __ENV.DURATION || "30s",
      gracefulStop: `${RESERVE_TIMEOUT + 5000}ms`,
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    reservation_latency: [`p(95)<${RESERVE_P95}`],
    reservation_timeouts: ["count==0"],
    reservation_wrong_status: ["count==0"],
    order_not_confirmed: ["count==0"],
    reservation_not_committed: ["count==0"],
    rpc_errors: ["count==0"],
    // the chain ran: orders created, reserved (latency samples), confirmed and committed
    "rpc_ok{rpc:CreateOrder}": ["count>0"],
    "rpc_ok{rpc:CompleteOrder}": ["count>0"],
    "rpc_ok{rpc:GetReservation}": ["count>0"],
  },
  summaryTrendStats: ["avg", "p(50)", "p(95)", "p(99)", "max"],
};

export function setup() {
  for (let i = 0; i < SKUS; i++) {
    setStock(i, 1_000_000);
  }
}

export default function () {
  const created = ok(
    createOrder(
      {
        items: [
          {
            sku: sku(Math.floor(Math.random() * SKUS)),
            quantity: 1,
            unit_price: { amount_minor: 1000, currency: "USD" },
          },
        ],
      },
      customer(`reserve-${__VU}`)
    ),
    "CreateOrder"
  );
  if (!created) return;
  const start = Date.now();
  for (;;) {
    // NotFound until the worker has consumed the order: not an error here.
    const res = rpc(
      "inventory.v1.InventoryService/GetReservation",
      { order_id: created.order.id },
      { tags: { poll: "true" } }
    );
    if (res.status === 200) {
      const status = res.json("reservation.status");
      if (status === "RESERVATION_STATUS_RESERVED") {
        reservationLatency.add(Date.now() - start);
        if (waitConfirmed(created.order.id, start)) completeAndWaitCommitted(created.order.id, start);
      } else {
        reservationWrong.add(1, { status: String(status) });
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

// The other half of the saga: ordering consumes StockReserved and the order turns
// CONFIRMED. Same deadline as the reservation (counted from CreateOrder).
function waitConfirmed(orderId, start) {
  for (;;) {
    const res = rpc("ordering.v1.OrderService/GetOrder", { id: orderId }, { tags: { poll: "true" } });
    if (!ok(res, "GetOrder")) return false;
    const status = res.json("order.status");
    if (status === "ORDER_STATUS_CONFIRMED") return true;
    if (status !== "ORDER_STATUS_PENDING" || Date.now() - start > RESERVE_TIMEOUT) {
      orderNotConfirmed.add(1, { status: String(status) });
      return false;
    }
    sleep(POLL / 1000);
  }
}

// And the sale: CompleteOrder (rpc) → OrderCompleted → inventory commits the
// reservation (COMMITTED). Same deadline again, so a slow worker shows up here too.
function completeAndWaitCommitted(orderId, start) {
  if (!ok(rpc("ordering.v1.OrderService/CompleteOrder", { id: orderId }), "CompleteOrder")) return;
  for (;;) {
    const res = rpc("inventory.v1.InventoryService/GetReservation", { order_id: orderId }, { tags: { poll: "true" } });
    if (!ok(res, "GetReservation")) return;
    const status = res.json("reservation.status");
    if (status === "RESERVATION_STATUS_COMMITTED") return;
    if (status !== "RESERVATION_STATUS_RESERVED" || Date.now() - start > RESERVE_TIMEOUT) {
      reservationNotCommitted.add(1, { status: String(status) });
      return;
    }
    sleep(POLL / 1000);
  }
}
