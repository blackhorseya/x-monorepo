-- Consumer inbox of the inventory BC (platform/postgres.Inbox, usecasex.Idempotent): one row per
-- event message id this BC's use cases have consumed, inserted inside the use case's
-- transaction. A redelivery (at-least-once broker, poison requeue, lost ack) hits the primary
-- key and is dropped before any aggregate is touched. Rows are kept: they must outlive the
-- longest possible redelivery window (a poison row can be requeued any time later).
CREATE TABLE inventory.inbox (
    message_id  VARCHAR(36)  PRIMARY KEY,
    consumed_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX inbox_consumed_at_idx ON inventory.inbox (consumed_at);
