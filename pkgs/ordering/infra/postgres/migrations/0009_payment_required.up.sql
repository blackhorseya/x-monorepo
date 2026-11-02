-- docs/payment.md §2, §8: the order's payment_required snapshot (ordering.require_payment as it was
-- when the order became CONFIRMED, never changed afterwards), and the topic table of the new
-- ordering.v1.OrderPaid event (same watermill-sql topic table + offsets pair as 0003_events.up.sql;
-- the schema is not auto-created, so the table has to exist before the first publish).
--
-- Every existing order was CONFIRMED (or not yet) before Payment existed: DEFAULT false gives them
-- the snapshot "no payment owed", which is what keeps them completable as before.
--
-- NOTE the whole migration runs in ONE transaction. ADD COLUMN with a constant DEFAULT is a
-- metadata-only change on PostgreSQL 11+ (no table rewrite, no scan), but it still takes ACCESS
-- EXCLUSIVE on ordering.orders until commit, so reads and writes of the table wait for the
-- migration; it is short because nothing here touches the rows.
ALTER TABLE ordering.orders
    ADD COLUMN payment_required boolean NOT NULL DEFAULT false;

CREATE TABLE ordering.events_v1_orderpaid (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_orderpaid_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
