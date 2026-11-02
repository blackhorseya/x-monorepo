-- Slice 5b of the payment BC (docs/payment.md §3「已知限制」): the mock provider's state, so that
-- `serve`, `consume` and `expire` — three processes — talk to one mock instead of three. Only the mock
-- uses these tables; a real provider keeps its state at the provider, and nothing here is read or
-- written when payment.provider is not "mock". Demo / kind data: nothing prunes it (docs/operations.md).
-- Only new tables, no ALTER of an existing one: the file takes no lock a running payment-rpc waits on.

CREATE TABLE payment.mock_checkouts (
    -- the provider's checkout id: the mock mints it (UUID v7), payment.payments.provider_ref keeps it
    ref           text        PRIMARY KEY CHECK (octet_length(ref) BETWEEN 1 AND 128),
    -- opening order, the one stable order of "every checkout ever opened"
    seq           bigserial   NOT NULL UNIQUE,
    payment_id    text        NOT NULL,
    amount        bigint      NOT NULL CHECK (amount >= 0),
    currency      char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    url           text        NOT NULL,
    expires_at    timestamptz NOT NULL,
    paid          boolean     NOT NULL DEFAULT false,
    -- what the customer paid (need not be the checkout's amount); set together with paid, in one statement
    paid_amount   bigint      CHECK (paid_amount >= 0),
    paid_currency char(3)     CHECK (paid_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT mock_checkouts_paid_has_amount CHECK (paid = (paid_amount IS NOT NULL AND paid_currency IS NOT NULL)),
    cancelled     boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per idempotency key the provider honoured: the primary key is what makes a refund with the
-- same key from any process refund once.
CREATE TABLE payment.mock_refunds (
    ref             text        NOT NULL REFERENCES payment.mock_checkouts (ref) ON DELETE CASCADE,
    idempotency_key text        NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ref, idempotency_key)
);
