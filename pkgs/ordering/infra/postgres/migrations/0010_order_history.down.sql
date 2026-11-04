-- Rolls back this BC only (ordering). The history is a record: dropping the table forgets it for good.
DROP TABLE IF EXISTS ordering.order_history;
