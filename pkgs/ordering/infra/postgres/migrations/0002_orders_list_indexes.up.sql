-- Keyset pagination for ListOrders: (created_at, id) is the sort key, customer_id
-- the common filter. Postgres scans these backwards for ORDER BY ... DESC.
CREATE INDEX orders_created_at_id_idx ON ordering.orders (created_at, id);
CREATE INDEX orders_customer_created_at_id_idx ON ordering.orders (customer_id, created_at, id);
