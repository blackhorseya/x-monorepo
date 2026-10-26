-- ExpireReservations scans RESERVED rows by the start of their hold (updated_at, which
-- Reserve sets equal to created_at). Partial: every other status is terminal and never read this way.
CREATE INDEX reservations_reserved_updated_at_idx ON inventory.reservations (updated_at, order_id) WHERE status = 'RESERVED';
