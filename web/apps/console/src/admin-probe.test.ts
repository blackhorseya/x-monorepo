import assert from "node:assert/strict";
import { afterEach, describe, it } from "node:test";
import { Code, ConnectError } from "@connectrpc/connect";
import { adminProbeQuery } from "./queries.ts";
import { server, type Answer } from "./testing/client.stub.ts";

//   node --test --experimental-strip-types --import ./src/testing/register.mjs src/admin-probe.test.ts   (task web-test)
//
// What adminProbeQuery answers for each thing the server can say, and WHICH call it makes to find out.
// The probe asks the cheapest read-only admin RPC, admin.v1.AdminService/ListPoison with limit 1
// (protos/admin/v1/service.proto). Its answer is the one the console acts on: true = show the admin pages,
// false = this credential is not an admin.
//
// pkgs/platform/admin/poison_service.go guard() checks the ROLE first and the DATABASE second, so
// FAILED_PRECONDITION with x-error-code admin.no_database (an rpc with no database: in-memory, the e2e `auth`
// and `image` projects) is only ever sent to a caller that already passed the role check: that is "admin",
// not an error. Any other FAILED_PRECONDITION says nothing about the role and is rethrown like every other
// failure (network, 5xx), for the shared retry policy (src/transient.ts) to handle.
//
// The query function is called directly (not through a QueryObserver), so a rethrown error is captured as
// the probe's outcome instead of being retried by react-query's timers; the retry policy of this query is
// src/queries.retry.test.ts's business.

type Outcome = { rpc: string[]; request: unknown[]; result: { value: unknown } | { error: string } };

async function probe(answer: Answer): Promise<Outcome> {
  server.reset();
  server.answer = () => answer;
  const queryFn = adminProbeQuery(0).queryFn as () => Promise<unknown>;
  let result: Outcome["result"];
  try {
    result = { value: await queryFn() };
  } catch (err) {
    const e = ConnectError.from(err);
    result = { error: `${Code[e.code]} ${e.metadata.get("x-error-code") ?? ""}`.trim() };
  }
  return { rpc: [...server.calls], request: [...server.requests], result };
}

const LIST_POISON_ONE = { rpc: ["admin.listPoison"], request: [{ limit: 1 }] };
const fail = (code: Code, errorCode?: string): Answer => ({
  fail: new ConnectError("scripted", code, errorCode ? { "x-error-code": errorCode } : undefined),
});

afterEach(() => server.reset());

describe("adminProbeQuery", () => {
  it("ListPoison answers -> true", async () => {
    assert.deepEqual(await probe({ ok: { messages: [] } }), { ...LIST_POISON_ONE, result: { value: true } });
  });
  it("PERMISSION_DENIED -> false (an answer: this credential is not an admin)", async () => {
    assert.deepEqual(await probe(fail(Code.PermissionDenied, "auth.permission_denied")), { ...LIST_POISON_ONE, result: { value: false } });
  });
  it("UNAUTHENTICATED -> false", async () => {
    assert.deepEqual(await probe(fail(Code.Unauthenticated, "auth.unauthenticated")), { ...LIST_POISON_ONE, result: { value: false } });
  });
  it("FAILED_PRECONDITION admin.no_database -> true (the role check came first and passed)", async () => {
    assert.deepEqual(await probe(fail(Code.FailedPrecondition, "admin.no_database")), { ...LIST_POISON_ONE, result: { value: true } });
  });
  it("any other FAILED_PRECONDITION is rethrown, not read as a role answer", async () => {
    assert.deepEqual(await probe(fail(Code.FailedPrecondition, "something.else")), {
      ...LIST_POISON_ONE,
      result: { error: "FailedPrecondition something.else" },
    });
    assert.deepEqual(await probe(fail(Code.FailedPrecondition)), { ...LIST_POISON_ONE, result: { error: "FailedPrecondition" } });
  });
  it("every other failure is rethrown (network, 5xx: the shared retry policy's to handle)", async () => {
    assert.deepEqual(await probe(fail(Code.Unavailable)), { ...LIST_POISON_ONE, result: { error: "Unavailable" } });
    assert.deepEqual(await probe(fail(Code.Internal, "internal")), { ...LIST_POISON_ONE, result: { error: "Internal internal" } });
  });
});
