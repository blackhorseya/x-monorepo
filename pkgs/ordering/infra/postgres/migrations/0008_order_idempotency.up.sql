-- CreateOrder idempotency: the caller's retry token and the fingerprint of the items it was
-- spent on live on the order itself, so they live exactly as long as the order. NULL = created
-- without a key (every pre-0008 row); nothing is backfilled.
--
-- NOTE the index build takes a SHARE lock on ordering.orders (writes wait, reads don't): migrations
-- run inside a transaction, so it cannot be CONCURRENTLY. On a large table run migrate in a
-- maintenance window; the partial predicate keeps the index itself empty until clients send keys.
ALTER TABLE ordering.orders
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_hash    text;

CREATE UNIQUE INDEX orders_customer_idempotency_key
    ON ordering.orders (customer_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
