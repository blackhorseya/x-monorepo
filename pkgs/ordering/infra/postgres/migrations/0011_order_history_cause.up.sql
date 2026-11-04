-- #427 slice 2: OrderCancellationRequested gains a typed cause (STOCK_SHORTAGE, RESERVATION_EXPIRED) and
-- the history records it, so the cause CHECK of ordering.order_history admits those two values.
--
-- Lock: the whole migration runs in ONE transaction, so the ACCESS EXCLUSIVE lock of ALTER TABLE is held
-- until commit; it falls on ordering.order_history alone. Nothing here touches ordering.orders (no FK, no
-- trigger), so reads and writes of orders do not wait for it, and ADD CONSTRAINT scans only the history
-- (a new, small, append-only table). The constraint is added validated, not NOT VALID + VALIDATE: both
-- would sit in the same transaction and hold the same lock, so splitting them buys nothing (#318).
--
-- The new CHECK is a superset of the old one, so every existing row satisfies it.
ALTER TABLE ordering.order_history DROP CONSTRAINT order_history_cause_check;
ALTER TABLE ordering.order_history ADD CONSTRAINT order_history_cause_check
    CHECK (cause IN ('UNSPECIFIED', 'RPC_REQUEST', 'PAYMENT_CAPTURED', 'PAYMENT_EXPIRED', 'STOCK_RESERVED', 'CANCELLATION_REQUESTED', 'STOCK_SHORTAGE', 'RESERVATION_EXPIRED'));
