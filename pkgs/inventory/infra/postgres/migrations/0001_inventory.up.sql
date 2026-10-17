CREATE TABLE inventory.stocks (
    sku        text        PRIMARY KEY,
    on_hand    int         NOT NULL CHECK (on_hand >= 0),
    reserved   int         NOT NULL CHECK (reserved >= 0 AND reserved <= on_hand),
    -- optimistic lock: Save updates only WHERE version matches, then bumps it
    version    bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL
);

-- One reservation per order (the order id is the identity): a redelivered
-- OrderCreated finds it and does nothing.
CREATE TABLE inventory.reservations (
    order_id   text        PRIMARY KEY,
    status     text        NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- short marks the lines whose SKU could not be covered (REJECTED only).
CREATE TABLE inventory.reservation_lines (
    order_id text    NOT NULL REFERENCES inventory.reservations (order_id) ON DELETE CASCADE,
    position int     NOT NULL,
    sku      text    NOT NULL,
    quantity int     NOT NULL,
    short    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (order_id, position)
);
