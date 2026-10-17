-- Event tables of the inventory BC: one watermill-sql (v4, Postgres) topic table + consumer
-- offsets table per topic, named by platform/eventbus.TableName (<bc>.events_<topic>).
-- Topics: inventory.v1.StockReserved / StockReleased (published), inventory.poison (messages
-- the router gave up on). The ordering.v1.* topics inventory consumes or publishes into
-- (OrderCreated, OrderCancelled, OrderCancellationRequested) live in ordering's migrations.

CREATE TABLE inventory.events_v1_stockreserved (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE inventory.events_v1_stockreserved_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE inventory.events_v1_stockreleased (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE inventory.events_v1_stockreleased_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE inventory.events_poison (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE inventory.events_poison_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
