import { randomUUID } from "node:crypto";

// A suffix for account names, SKUs and other ids that stays unique when specs, and --repeat-each
// copies of them, run in parallel: Date.now() alone repeats within the same millisecond, and
// two accounts under one name make the "one row" assertions see two.
export const uid = () => `${Date.now()}-${randomUUID().slice(0, 8)}`;
