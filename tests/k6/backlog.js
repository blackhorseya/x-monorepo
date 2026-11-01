// Fills the ordering outbox while no worker runs: exactly ORDERS CreateOrder calls (default 2000),
// no polling for a reservation. subscribe_batch.sh (#363) uses it to build the backlog its
// metric B drains.
//   k6 run tests/k6/backlog.js -e BASE_URL=… [-e ORDERS=2000] [-e VUS=20] [-e SKUS=20]
import { ok, sku, setStock, upsertProduct, createOrder, customer, systemTags, throwOnError } from "./lib.js";

const SKUS = Number(__ENV.SKUS || 20);

export const options = {
  throw: throwOnError,
  systemTags,
  scenarios: {
    backlog: {
      executor: "shared-iterations",
      vus: Number(__ENV.VUS || 20),
      iterations: Number(__ENV.ORDERS || 2000),
      maxDuration: "10m",
    },
  },
  thresholds: { rpc_errors: ["count==0"], http_req_failed: ["rate==0"], "rpc_ok{rpc:CreateOrder}": ["count>0"] },
};

export function setup() {
  for (let i = 0; i < SKUS; i++) {
    upsertProduct(i, 1000);
    setStock(i, 1_000_000);
  }
}

export default function () {
  ok(createOrder({ items: [{ sku: sku(Math.floor(Math.random() * SKUS)), quantity: 1 }] }, customer(`backlog-${__VU}`)), "CreateOrder");
}
