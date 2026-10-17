-- Optimistic concurrency for the Order aggregate: Save inserts with version 1
-- and updates only `WHERE version = <loaded>`, bumping it. Existing rows were
-- saved at least once, so they start at 1 like a fresh insert.
ALTER TABLE ordering.orders ADD COLUMN version bigint NOT NULL DEFAULT 1;
