-- Keyset pagination for ListReservations: (created_at, order_id) is the sort key,
-- status the only filter. Postgres scans these backwards for ORDER BY ... DESC.
CREATE INDEX reservations_created_at_order_id_idx ON inventory.reservations (created_at, order_id);
CREATE INDEX reservations_status_created_at_order_id_idx ON inventory.reservations (status, created_at, order_id);
