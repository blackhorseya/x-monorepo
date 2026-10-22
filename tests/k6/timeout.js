// The timeout interceptor under load. Run the server with a deadline no RPC can
// meet, e.g. APP_HTTP_TIMEOUT=1ns (needs postgres.url: a DB call is where an
// expired context surfaces; the in-memory repositories never look at it), then
//   k6 run tests/k6/timeout.js [-e VUS=10] [-e DURATION=5s]
// Expected: every call is DEADLINE_EXCEEDED (HTTP 504) with x-error-code
// request.timeout — never INTERNAL, whatever the DB driver wrapped the context
// error in — and the server keeps answering (no connection errors).
import http from "k6/http";
import { check, sleep } from "k6";
import { Rate } from "k6/metrics";
import { rpc, errorCode } from "./lib.js";

// 504 is the expected answer; a connection error or anything else is a failed request.
http.setResponseCallback(http.expectedStatuses(504));

const timedOut = new Rate("timed_out");

export const options = {
  scenarios: {
    deadline: { executor: "constant-vus", vus: Number(__ENV.VUS || 10), duration: __ENV.DURATION || "5s" },
  },
  thresholds: {
    timed_out: ["rate==1"],
    http_req_failed: ["rate==0"],
    http_reqs: ["count>0"],
  },
};

export default function () {
  const res = rpc("ordering.v1.OrderService/ListOrders", { page_size: 1 });
  timedOut.add(
    check(res, {
      "504": (r) => r.status === 504,
      "code deadline_exceeded": (r) => r.json("code") === "deadline_exceeded",
      "x-error-code request.timeout": (r) => errorCode(r) === "request.timeout",
    }),
  );
  sleep(0.01);
}
