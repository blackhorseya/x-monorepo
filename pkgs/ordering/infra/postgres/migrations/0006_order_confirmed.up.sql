-- ordering.v1.OrderConfirmed (published: PENDING → CONFIRMED once inventory.v1.StockReserved
-- is consumed): the same watermill-sql topic table + offsets pair as 0003_events.up.sql.
-- The StockReserved table this BC now consumes belongs to inventory's migrations.

CREATE TABLE ordering.events_v1_orderconfirmed (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_orderconfirmed_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
