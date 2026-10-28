import { randomUUID } from "node:crypto";

// A suffix for customer ids, SKUs and names that stays unique when specs, and --repeat-each
// copies of them, run in parallel: Date.now() alone repeats within the same millisecond, and
// two orders under one customer id make the "one row" assertions see two.
export const uid = () => `${Date.now()}-${randomUUID().slice(0, 8)}`;
