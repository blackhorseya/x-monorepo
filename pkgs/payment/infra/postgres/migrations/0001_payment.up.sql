-- SPEC STAGE: the columns, none of the checks or the unique constraint.
CREATE TABLE payment.payments (
    id                  text        PRIMARY KEY,
    order_id            text        NOT NULL,
    customer_id         text        NOT NULL,
    amount              bigint      NOT NULL,
    currency            text        NOT NULL,
    status              text        NOT NULL,
    order_outcome       text        NOT NULL DEFAULT 'NONE',
    expires_at          timestamptz NOT NULL,
    provider            text        NOT NULL DEFAULT '',
    provider_ref        text        NOT NULL DEFAULT '',
    checkout_url        text        NOT NULL DEFAULT '',
    checkout_expires_at timestamptz,
    terminal_at         timestamptz,
    version             bigint      NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL,
    CONSTRAINT payments_outcome_fits_status CHECK (
        (status IN ('PENDING', 'CAPTURED') AND order_outcome = 'NONE')
        OR (status IN ('CANCELLED', 'REFUNDING', 'REFUNDED') AND order_outcome <> 'NONE')
        OR status IN ('EXPIRED', 'NEEDS_REVIEW'))
);
