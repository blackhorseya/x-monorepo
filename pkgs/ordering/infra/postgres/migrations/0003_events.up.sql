-- Event tables of the ordering BC: one watermill-sql (v4, Postgres) topic table + consumer
-- offsets table per topic, exactly the DDL the library would create with InitializeSchema,
-- named by platform/eventbus.TableName (<bc>.events_<topic>). Payload is proto bytes.
-- Topics: ordering.v1.OrderCreated / OrderCancelled (published; OrderConfirmed came in 0006), ordering.v1.OrderCancellationRequested
-- (consumed), ordering.poison (messages the router gave up on).

CREATE TABLE ordering.events_v1_ordercreated (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_ordercreated_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE ordering.events_v1_ordercancelled (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_ordercancelled_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE ordering.events_v1_ordercancellationrequested (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_v1_ordercancellationrequested_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE ordering.events_poison (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE ordering.events_poison_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);
