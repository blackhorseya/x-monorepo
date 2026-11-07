import { Code, ConnectError } from "@connectrpc/connect";

// The one place that decides whether a failed read is worth asking again. Every query uses it
// (queryClient.ts sets it as the default, refreshFirstPage consults it); no query carries its own `retry`.
//
// Transient = the request may well succeed unchanged a moment later:
//   Unavailable        the server (or a proxy in front of it: 502/503/504/429 map here) is not answering
//   DeadlineExceeded   it did not answer in time
//   ResourceExhausted  rate limited / overloaded; backs off like the others
//   a network error    fetch itself failed (the connection reset, the server unreachable). connect-web
//                      turns that TypeError into Code.Unknown with the TypeError as `cause`, so Unknown is
//                      transient ONLY with that cause: a bare Unknown is a server answer we cannot read.
// Everything else is an answer (PermissionDenied, Unauthenticated, NotFound, InvalidArgument,
// FailedPrecondition, ...): asking again returns the same thing, so the error is shown at once.
// Decided by the Connect code, never by the message text.
export function isTransient(err: unknown): boolean {
  if (err instanceof TypeError) return true; // a fetch failure that was not wrapped
  const e = ConnectError.from(err);
  switch (e.code) {
    case Code.Unavailable:
    case Code.DeadlineExceeded:
    case Code.ResourceExhausted:
      return true;
    case Code.Unknown:
      return e.cause instanceof TypeError;
    default:
      return false;
  }
}

// The bound, so a server that stays down costs a short, fixed wait and not an endless loop:
// at most MAX_RETRIES retries (MAX_RETRIES + 1 requests), waiting 500 ms, then 1 s, then 2 s
// (doubling, capped at MAX_RETRY_DELAY_MS): 3.5 s in all before the error is shown.
export const MAX_RETRIES = 3;
export const BASE_RETRY_DELAY_MS = 500;
export const MAX_RETRY_DELAY_MS = 2000;

// react-query's `retry`: `failureCount` is the number of failures so far (0 on the first).
export function shouldRetry(failureCount: number, err: unknown): boolean {
  return failureCount < MAX_RETRIES && isTransient(err);
}

// react-query's `retryDelay`: `failureCount` is 0-based here too.
export function retryDelay(failureCount: number): number {
  return Math.min(BASE_RETRY_DELAY_MS * 2 ** failureCount, MAX_RETRY_DELAY_MS);
}
