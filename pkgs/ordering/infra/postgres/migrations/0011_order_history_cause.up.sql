-- #427 slice 2: OrderCancellationRequested gains a typed cause (STOCK_SHORTAGE, RESERVATION_EXPIRED) and
-- the history records it, so the cause CHECK of ordering.order_history admits those two values.
--
-- Lock: the file runs as ONE statement batch, i.e. one transaction, so the ACCESS EXCLUSIVE lock of
-- ALTER TABLE is held until commit. It is on ordering.order_history, but that is NOT a lock the order
-- paths can ignore: CancelOrder, ConfirmOrder, CompleteOrder and the payment events UPDATE ordering.orders
-- and then INSERT into order_history in the same transaction, and GetOrderHistory reads it. While this
-- migration holds (or waits for) the lock, those requests wait, and a request that outlives its deadline
-- rolls back whole (measured, PR #438 review; see docs/operations.md).
--
-- So the migration keeps the lock short and refuses to queue for it:
--  * ADD CONSTRAINT ... NOT VALID: the new CHECK is a superset of the old one, so every existing row
--    satisfies it by construction and there is nothing to validate. NOT VALID skips the full-table scan
--    (so the hold does not grow with the history); it still enforces the CHECK on every new or updated row.
--    There is deliberately no VALIDATE CONSTRAINT: it would add nothing here, and in this same transaction
--    it would hold the same lock again (#318). Should one ever be wanted, it is a separate migration.
--  * SET LOCAL lock_timeout: an ALTER TABLE that cannot get its lock waits in the lock queue, and every
--    later statement on the table queues behind it; a long transaction holding the table would therefore
--    stall the whole order write path, not just delay this migration. With the timeout the migration fails
--    after 3 seconds instead, the order paths are released, and the operator retries at a quieter moment.
--    3s is below the 10s default of http.timeout, so a request that queued behind the waiting ALTER
--    is released before it times out. SET LOCAL lasts for this transaction only.
-- A timed-out run leaves schema_migrations at 11 dirty (golang-migrate marks the version before it runs
-- the file) with the table unchanged; docs/operations.md says how to reset and rerun.
SET LOCAL lock_timeout = '3s';
ALTER TABLE ordering.order_history DROP CONSTRAINT order_history_cause_check;
ALTER TABLE ordering.order_history ADD CONSTRAINT order_history_cause_check
    CHECK (cause IN ('UNSPECIFIED', 'RPC_REQUEST', 'PAYMENT_CAPTURED', 'PAYMENT_EXPIRED', 'STOCK_RESERVED', 'CANCELLATION_REQUESTED', 'STOCK_SHORTAGE', 'RESERVATION_EXPIRED')) NOT VALID;
