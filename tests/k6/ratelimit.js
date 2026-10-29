// The rate limit interceptor under real traffic. Run the server with
//   APP_HTTP_RATE_LIMIT_RPS=<RPS> APP_HTTP_RATE_LIMIT_BURST=<RPS>   (by: global)
// and this script at a multiple of it:
//   k6 run tests/k6/ratelimit.js -e RPS=50 [-e FACTOR=4] [-e DURATION=10s]
// Expected: every rejected call is RESOURCE_EXHAUSTED (HTTP 429) with
// x-error-code request.rate_limited and an integer Retry-After ≥ 1; the
// accepted calls come through at about RPS (bucket burst on top), the rest
// are rejected — the limiter rejects, it never queues.
import http from "k6/http";
import { check } from "k6";
import { Counter, Rate } from "k6/metrics";
import { rpc, knownCode, systemTags, throwOnError } from "./lib.js";

const RPS = Number(__ENV.RPS || 50);
const FACTOR = Number(__ENV.FACTOR || 4);
const DURATION = __ENV.DURATION || "10s";
const seconds = Number(DURATION.replace(/s$/, ""));

// 429 is the point of this script: only anything else counts as a failed request.
http.setResponseCallback(http.expectedStatuses(200, 429));

const accepted = new Counter("accepted");
const rejected = new Counter("rate_limited");
const wellFormed = new Rate("rate_limited_well_formed");

export const options = {
  throw: throwOnError,
  systemTags,
  scenarios: {
    flood: {
      executor: "constant-arrival-rate",
      rate: RPS * FACTOR,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: Math.max(10, RPS * FACTOR),
    },
  },
  thresholds: {
    // the limiter let through about RPS/s (+ one burst), and rejected the rest
    accepted: [
      `count<=${Math.ceil(RPS * seconds * 1.3 + RPS)}`,
      `count>=${Math.floor(RPS * seconds * 0.7)}`,
    ],
    rate_limited: ["count>0"],
    rate_limited_well_formed: ["rate==1"],
    // nothing but 200 and 429 came back
    http_req_failed: ["rate==0"],
  },
};

export default function () {
  const res = rpc(
    "ordering.v1.OrderService/ListOrders",
    { page_size: 1 },
    { tags: { scenario: "flood" } }
  );
  if (res.status === 200) {
    accepted.add(1);
    return;
  }
  rejected.add(1);
  const retryAfter = Number(res.headers["Retry-After"]);
  wellFormed.add(
    check(res, {
      429: (r) => r.status === 429,
      "code resource_exhausted": (r) => r.json("code") === "resource_exhausted",
      "x-error-code request.rate_limited": (r) =>
        knownCode(r) === "request.rate_limited",
      "Retry-After integer >= 1": () =>
        Number.isInteger(retryAfter) && retryAfter >= 1,
    })
  );
}
