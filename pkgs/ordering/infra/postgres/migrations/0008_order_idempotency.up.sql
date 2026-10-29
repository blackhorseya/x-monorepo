-- CreateOrder idempotency: the caller's retry token and the fingerprint of the items it was
-- spent on live on the order itself, so they live exactly as long as the order. NULL = created
-- without a key (every pre-0008 row); nothing is backfilled.
--
-- NOTE the whole migration runs in ONE transaction. ALTER TABLE takes ACCESS EXCLUSIVE on
-- ordering.orders and holds it until commit, so the index build below (a table scan; it cannot be
-- CONCURRENTLY inside a transaction) runs under it: reads AND writes of the table wait for the
-- whole migration, not just the index. On a large table run migrate in a maintenance window that
-- covers read traffic too; the partial predicate keeps the index itself empty until clients send keys.
ALTER TABLE ordering.orders
    ADD COLUMN idempotency_key text,
    ADD COLUMN request_hash    text;

CREATE UNIQUE INDEX orders_customer_idempotency_key
    ON ordering.orders (customer_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
