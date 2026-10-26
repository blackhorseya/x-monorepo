-- inventory.v1.StockCommitted (published: RESERVED → COMMITTED when ordering.v1.OrderCompleted
-- is consumed; the stock is sold): the same watermill-sql topic table + offsets pair as
-- 0002_events.up.sql. The OrderCompleted table this BC now consumes belongs to ordering's migrations.

CREATE TABLE inventory.events_v1_stockcommitted (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE inventory.events_v1_stockcommitted_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
