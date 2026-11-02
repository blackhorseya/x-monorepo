-- Slice 3 of the payment BC (docs/payment.md §10): the event tables (outbox of the five payment.v1
-- events, the poison table), the consumer inbox, the webhook log and the order-outcome tombstones.
-- The event tables are exactly the DDL watermill-sql (v4, Postgres) would create, named by
-- platform/eventbus.TableName (<bc>.events_<topic>); payload is proto bytes.
-- Topics: payment.v1.PaymentCaptured / PaymentExpired / PaymentRefunded (published for others),
-- payment.v1.PaymentRefundRequested / WebhookReceived (payment's own work items), payment.poison.
-- The ordering.v1.Order* topics payment consumes keep their tables in ordering's schema; payment
-- only puts a consumer-group cursor on them.

CREATE TABLE payment.events_v1_paymentcaptured (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_v1_paymentcaptured_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE payment.events_v1_paymentexpired (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_v1_paymentexpired_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE payment.events_v1_paymentrefunded (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_v1_paymentrefunded_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE payment.events_v1_paymentrefundrequested (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_v1_paymentrefundrequested_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE payment.events_v1_webhookreceived (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_v1_webhookreceived_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

CREATE TABLE payment.events_poison (
    "offset"         BIGSERIAL,
    "uuid"           VARCHAR(36) NOT NULL,
    "created_at"     TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "payload"        BYTEA       DEFAULT NULL,
    "metadata"       JSON        DEFAULT NULL,
    "transaction_id" xid8        NOT NULL,
    PRIMARY KEY ("transaction_id", "offset")
);
CREATE TABLE payment.events_poison_offsets (
    consumer_group                VARCHAR(255) NOT NULL,
    offset_acked                  BIGINT,
    last_processed_transaction_id xid8 NOT NULL,
    PRIMARY KEY (consumer_group)
);

-- Consumer inbox (platform/postgres.Inbox, usecasex.Idempotent): one row per consumed message id,
-- inserted inside the use case's transaction. Rows are kept: they must outlive the longest
-- possible redelivery (a poison row can be requeued any time later).
CREATE TABLE payment.inbox (
    message_id  VARCHAR(36)  PRIMARY KEY,
    consumed_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX inbox_consumed_at_idx ON payment.inbox (consumed_at);

-- Verified provider notifications, written by the webhook route (signature checked over the raw
-- body BEFORE anything is written) in the same transaction that puts WebhookReceived in the outbox.
-- (provider, provider_event_id) is the dedup key: a redelivery inserts nothing and is answered 200.
-- processed_at stays NULL until `consume` has applied it; retention (payment.webhook_retention)
-- deletes only processed rows, so an unprocessed or failed one is never lost to a clean-up.
CREATE TABLE payment.webhook_events (
    provider          text        NOT NULL CHECK (octet_length(provider) BETWEEN 1 AND 64),
    provider_event_id text        NOT NULL CHECK (octet_length(provider_event_id) BETWEEN 1 AND 128),
    event_type        text        NOT NULL CHECK (octet_length(event_type) BETWEEN 1 AND 128),
    provider_ref      text        NOT NULL DEFAULT '' CHECK (octet_length(provider_ref) <= 128),
    -- the amount the provider reports, only for payment.captured (NULL otherwise)
    amount            bigint      CHECK (amount >= 0),
    currency          char(3)     CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT webhook_events_amount_with_currency CHECK ((amount IS NULL) = (currency IS NULL)),
    -- the raw body as received; bounded by the route (1 MiB) before it is read
    body              bytea       NOT NULL CHECK (octet_length(body) <= 1048576),
    received_at       timestamptz NOT NULL,
    processed_at      timestamptz,
    PRIMARY KEY (provider, provider_event_id)
);
-- retention scans processed rows oldest first
CREATE INDEX webhook_events_processed_idx ON payment.webhook_events (processed_at) WHERE processed_at IS NOT NULL;

-- Tombstones for OrderCancelled / OrderCompleted that arrive before OrderConfirmed (no ordering
-- between topics): T15 writes one, T14 reads it when the payment is finally created.
CREATE TABLE payment.order_outcomes (
    order_id    text        PRIMARY KEY CHECK (octet_length(order_id) BETWEEN 1 AND 64),
    outcome     text        NOT NULL CHECK (outcome IN ('CANCELLED', 'COMPLETED_UNPAID')),
    recorded_at timestamptz NOT NULL
);

-- The expire sweep: PENDING payments whose window closed, and EXPIRED / CANCELLED ones still inside
-- the reconcile window that have a provider reference.
CREATE INDEX payments_pending_due_idx ON payment.payments (expires_at) WHERE status = 'PENDING';
CREATE INDEX payments_reconcile_idx ON payment.payments (terminal_at)
    WHERE status IN ('EXPIRED', 'CANCELLED') AND provider_ref <> '';
-- webhook processing finds the payment by the provider's reference
CREATE INDEX payments_provider_ref_idx ON payment.payments (provider, provider_ref) WHERE provider_ref <> '';
