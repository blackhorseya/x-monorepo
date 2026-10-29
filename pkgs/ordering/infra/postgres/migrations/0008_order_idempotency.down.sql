DROP INDEX IF EXISTS ordering.orders_customer_idempotency_key;

ALTER TABLE ordering.orders
    DROP COLUMN IF EXISTS request_hash,
    DROP COLUMN IF EXISTS idempotency_key;
