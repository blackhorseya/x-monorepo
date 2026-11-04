-- Rolls back this BC only (ordering). A row that carries one of the two inventory causes would violate the
-- 0010 CHECK, so it is first re-labelled CANCELLATION_REQUESTED: the same "another BC asked, reason not
-- classified" that an older ordering writes for the same event. The rows themselves are kept.
-- Like the up file this holds a lock the order write path waits on (it appends to order_history in the
-- same transaction as the order), so it too gives up after 3 seconds rather than queue for it.
SET LOCAL lock_timeout = '3s';
UPDATE ordering.order_history SET cause = 'CANCELLATION_REQUESTED' WHERE cause IN ('STOCK_SHORTAGE', 'RESERVATION_EXPIRED');
ALTER TABLE ordering.order_history DROP CONSTRAINT IF EXISTS order_history_cause_check;
ALTER TABLE ordering.order_history ADD CONSTRAINT order_history_cause_check
    CHECK (cause IN ('UNSPECIFIED', 'RPC_REQUEST', 'PAYMENT_CAPTURED', 'PAYMENT_EXPIRED', 'STOCK_RESERVED', 'CANCELLATION_REQUESTED'));
