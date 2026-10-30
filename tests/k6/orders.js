// Load: the ordering write path and the two reads behind the console.
//   k6 run tests/k6/orders.js -e BASE_URL=http://127.0.0.1:8080 [-e API_KEY=…] [-e VUS=20] [-e DURATION=60s]
// setup() stocks SKUS skus (SetStock is idempotent: on_hand is a count) and makes each a catalog
// product (the server prices the lines; UpsertProduct replaces, so it is idempotent too), then each
// VU loops CreateOrder (1–3 lines) → GetOrder → ListOrders (who names the customer: K6_CUSTOMER_MODE, see lib.js). With the
// worker running, inventory reserves stock for every order; this script does
// not wait for that (see the worker's eventbus_consumer_lag for the backlog).
// Thresholds are what a laptop with Postgres in Docker comfortably meets; raise
// them on purpose, not by editing when they fail. The one knob is CREATE_P95
// (ms, default 300): smoke.sh sets it for CI, where a shared runner and a
// Postgres service container make the write path's tail wider, not slower on
// median (observed p50 22ms / p95 368ms / zero errors on a 10s run).
import { sleep } from "k6";
import { rpc, ok, sku, setStock, upsertProduct, customerId, createOrder, customer as customerName, systemTags, throwOnError } from "./lib.js";

const SKUS = Number(__ENV.SKUS || 20);
const CREATE_P95 = Number(__ENV.CREATE_P95 || 300);

export const options = {
  throw: throwOnError,
  systemTags,
  scenarios: {
    orders: {
      executor: "constant-vus",
      vus: Number(__ENV.VUS || 20),
      duration: __ENV.DURATION || "60s",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    "rpc_duration{rpc:CreateOrder}": [`p(95)<${CREATE_P95}`, "p(99)<800"],
    "rpc_duration{rpc:GetOrder}": ["p(95)<100"],
    "rpc_duration{rpc:ListOrders}": ["p(95)<200"],
    rpc_errors: ["count==0"],
    // something succeeded (an all-refused run has no errors to speak of if it never asked)
    "rpc_ok{rpc:CreateOrder}": ["count>0"],
    "rpc_ok{rpc:GetOrder}": ["count>0"],
    "rpc_ok{rpc:ListOrders}": ["count>0"],
  },
  summaryTrendStats: ["avg", "p(50)", "p(95)", "p(99)", "max"],
};

export function setup() {
  for (let i = 0; i < SKUS; i++) {
    setStock(i, 1_000_000);
    upsertProduct(i, 100 + i * 10);
  }
}

export default function () {
  const customer = customerName(`customer-${__VU}`);
  const n = 1 + Math.floor(Math.random() * 3);
  const items = [];
  for (let i = 0; i < n; i++) {
    items.push({
      sku: sku(Math.floor(Math.random() * SKUS)),
      quantity: 1 + Math.floor(Math.random() * 5),
    });
  }
  const created = ok(
    createOrder({ items }, customer),
    "CreateOrder"
  );
  if (created) {
    ok(
      rpc("ordering.v1.OrderService/GetOrder", { id: created.order.id }),
      "GetOrder"
    );
  }
  ok(
    rpc("ordering.v1.OrderService/ListOrders", {
      ...customerId(customer),
      page_size: 20,
    }),
    "ListOrders"
  );
  sleep(Number(__ENV.THINK || 0.1));
}
