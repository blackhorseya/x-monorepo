-- Rolls back this BC only (ordering). Dropping the column forgets every snapshot: after a down,
-- an order that was CONFIRMED with payment_required = true is completable again.
DROP TABLE IF EXISTS ordering.events_v1_orderpaid_offsets;
DROP TABLE IF EXISTS ordering.events_v1_orderpaid;
ALTER TABLE ordering.orders DROP COLUMN IF EXISTS payment_required;
