-- ordering.v1.OrderCompleted (published: CONFIRMED → COMPLETED on CompleteOrder; inventory
-- consumes it to commit the reservation): the same watermill-sql topic table + offsets pair
-- as 0003_events.up.sql.

CREATE TABLE ordering.events_v1_ordercompleted (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_ordercompleted_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
