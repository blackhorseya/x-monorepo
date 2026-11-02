-- Payments: the single payment of one order (docs/payment.md §3, §10). Slice 2 holds the payment
-- itself; the event tables (outbox / inbox / poison), the webhook log and the order_outcomes
-- tombstones arrive with the consumer in slice 3.
--
-- The checks keep the domain's invariants true for a row nobody wrote through the aggregate:
-- whatever the database accepts, GetPayment / StartPayment can rebuild (Reconstitute refuses the
-- same combinations).
CREATE TABLE payment.payments (
    -- UUID v7 (#365); also the idempotency key of the provider's refund call
    id                  text        PRIMARY KEY CHECK (octet_length(id) BETWEEN 1 AND 64),
    -- one payment per order; the consumer's redelivery of OrderConfirmed hits this and is ignored
    order_id            text        NOT NULL UNIQUE CHECK (octet_length(order_id) BETWEEN 1 AND 64),
    customer_id         text        NOT NULL CHECK (octet_length(customer_id) BETWEEN 1 AND 128),
    amount              bigint      NOT NULL CHECK (amount >= 0),
    currency            char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status              text        NOT NULL CHECK (status IN
        ('PENDING', 'CAPTURED', 'EXPIRED', 'CANCELLED', 'REFUNDING', 'REFUNDED', 'NEEDS_REVIEW')),
    -- what Payment knows about the order's end: NONE until OrderCancelled / OrderCompleted is heard
    order_outcome       text        NOT NULL DEFAULT 'NONE' CHECK (order_outcome IN
        ('NONE', 'CANCELLED', 'COMPLETED_UNPAID')),
    -- a payment that waits or holds money never heard the order end; one cancelled or being refunded always did
    CONSTRAINT payments_outcome_fits_status CHECK (
        (status IN ('PENDING', 'CAPTURED') AND order_outcome = 'NONE')
        OR (status IN ('CANCELLED', 'REFUNDING', 'REFUNDED') AND order_outcome <> 'NONE')
        OR status IN ('EXPIRED', 'NEEDS_REVIEW')),
    expires_at          timestamptz NOT NULL,
    -- set by StartPayment; '' / NULL until then
    provider            text        NOT NULL DEFAULT '',
    provider_ref        text        NOT NULL DEFAULT '',
    checkout_url        text        NOT NULL DEFAULT '',
    checkout_expires_at timestamptz,
    -- when it entered EXPIRED / CANCELLED: the reconcile window runs from it
    terminal_at         timestamptz,
    -- optimistic lock: Save updates only WHERE version matches, then bumps it
    version             bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL
);
