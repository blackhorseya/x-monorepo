-- #427: the append-only history of an order's transitions (who, when, from which status to which,
-- why). It is a record, not a delivery channel: ordering.events_v1_* (the outbox) is pruned and
-- carries no actor, so it cannot be this. `worker prune` only touches <bc>.events_* and <bc>.inbox
-- and never this table.
--
-- Only CREATE TABLE: no foreign key to ordering.orders. A foreign key would take a lock on orders,
-- and the whole migration runs in ONE transaction, so that lock would be held until commit (#318).
-- Whether to add one is a separate decision that starts with measuring the lock.
--
-- seq: 1..N per order, in write order, never reused. It is NOT orders.version: ApplyPaymentCaptured
-- writes PAID and COMPLETED in a single Save, i.e. two rows for one version. The writer assigns it
-- with `INSERT ... SELECT COALESCE(MAX(seq), 0) + 1 ... WHERE order_id = $1` inside the transaction
-- that already did the order's Save. Save's UPDATE holds the orders row lock until commit, so two
-- writers of one order are serialised and the loser fails its version check before it gets here;
-- MAX(seq) is therefore only correct when read AFTER Save. The primary key is the backstop: a writer
-- that skipped Save collides on (order_id, seq) and fails, which is an error, never a silent duplicate.
--
-- occurred_at is the event's time (the domain clock), not now(). from_status is NULL for CREATED.
-- actor_subject is the account id of a CUSTOMER or ADMIN and '' for SYSTEM / ANONYMOUS; readers other
-- than admins must not be shown it. cause is a fixed enumeration; an upstream's free text is never
-- stored. Orders that existed before this migration have no history (no backfill).
CREATE TABLE ordering.order_history (
    order_id      text        NOT NULL,
    seq           integer     NOT NULL CHECK (seq >= 1),
    event         text        NOT NULL CHECK (event IN ('CREATED', 'CONFIRMED', 'PAID', 'COMPLETED', 'CANCELLED')),
    from_status   text        CHECK (from_status IN ('PENDING', 'CONFIRMED', 'PAID', 'COMPLETED', 'CANCELLED')),
    to_status     text        NOT NULL CHECK (to_status IN ('PENDING', 'CONFIRMED', 'PAID', 'COMPLETED', 'CANCELLED')),
    occurred_at   timestamptz NOT NULL,
    actor_kind    text        NOT NULL CHECK (actor_kind IN ('CUSTOMER', 'ADMIN', 'SYSTEM', 'ANONYMOUS')),
    actor_subject text        NOT NULL DEFAULT '',
    cause         text        NOT NULL CHECK (cause IN ('UNSPECIFIED', 'RPC_REQUEST', 'PAYMENT_CAPTURED', 'PAYMENT_EXPIRED', 'STOCK_RESERVED', 'CANCELLATION_REQUESTED')),
    PRIMARY KEY (order_id, seq)
);
