import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { isTransient, MAX_RETRIES, MAX_RETRY_DELAY_MS, retryDelay, shouldRetry } from "./transient.ts";

//   node --test --experimental-strip-types src/transient.test.ts      (task web-test)
// One row per Connect code: a code added to the enum later fails the count below until it is classified.
const transient = new Set<Code>([Code.Unavailable, Code.DeadlineExceeded, Code.ResourceExhausted]);
const codes = Object.values(Code).filter((c): c is Code => typeof c === "number");

describe("isTransient by Connect code", () => {
  it("covers every code in the enum (16)", () => assert.equal(codes.length, 16));
  for (const code of codes) {
    // Unknown on its own is NOT transient (see the network-error cases below); the rest follow the set.
    const want = transient.has(code);
    it(`${Code[code]} -> ${want}`, () => assert.equal(isTransient(new ConnectError("x", code)), want));
  }
});

describe("isTransient on what fetch throws", () => {
  it("Unknown caused by a TypeError (connect-web's wrapping of a reset connection) is transient", () => {
    assert.equal(isTransient(ConnectError.from(new TypeError("Failed to fetch"))), true);
  });
  it("a bare TypeError is transient", () => assert.equal(isTransient(new TypeError("Failed to fetch")), true));
  it("Unknown caused by another error is not", () => {
    assert.equal(isTransient(ConnectError.from(new RangeError("x"))), false);
  });
  it("Unknown with no cause is not", () => assert.equal(isTransient(new ConnectError("x", Code.Unknown)), false));
  it("an abort (Canceled) is not", () => {
    const abort = new Error("aborted");
    abort.name = "AbortError";
    assert.equal(isTransient(ConnectError.from(abort)), false);
  });
  it("is decided by the code, not the message", () => {
    assert.equal(isTransient(new ConnectError("service unavailable, try again", Code.PermissionDenied)), false);
    assert.equal(isTransient(new ConnectError("permission denied", Code.Unavailable)), true);
  });
  it("a non-error value is not", () => assert.equal(isTransient("boom"), false));
});

describe("shouldRetry / retryDelay", () => {
  const blip = new ConnectError("x", Code.Unavailable);
  it("retries a transient error up to MAX_RETRIES times, then stops", () => {
    for (let n = 0; n < MAX_RETRIES; n++) assert.equal(shouldRetry(n, blip), true, `failure ${n}`);
    assert.equal(shouldRetry(MAX_RETRIES, blip), false);
    assert.equal(shouldRetry(100, blip), false);
  });
  it("never retries an answer", () => assert.equal(shouldRetry(0, new ConnectError("x", Code.PermissionDenied)), false));
  it("backs off exponentially, capped, total bounded", () => {
    assert.deepEqual([0, 1, 2, 3, 4, 10].map(retryDelay), [500, 1000, 2000, 2000, 2000, 2000]);
    let total = 0;
    for (let n = 0; n < MAX_RETRIES; n++) total += retryDelay(n);
    assert.ok(total <= MAX_RETRIES * MAX_RETRY_DELAY_MS);
    assert.equal(total, 3500);
  });
});
