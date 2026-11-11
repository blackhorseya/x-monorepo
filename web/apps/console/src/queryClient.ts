import { QueryClient } from "@tanstack/react-query";
import { retryDelay, shouldRetry } from "./transient";

// One client for the app, importable outside React: the router's preload hooks run before any
// component renders and need to read what the queries already know.
export const queryClient = new QueryClient({
  defaultOptions: {
    // A read retries only when the failure is transient (503, a timeout, a reset connection), a few
    // times with a short bounded backoff; a domain error (PermissionDenied, NotFound,
    // InvalidArgument, ...) is an answer and shows at once. One classifier for every query
    // (src/transient.ts): a query does not set its own `retry`.
    queries: { retry: shouldRetry, retryDelay },
    // A mutation is never retried by the console: the admin writes change state, so
    // re-sending one on a guess can repeat its effect (RequeuePoison, Prune): "one click, one effect".
    mutations: { retry: 0 },
  },
});
