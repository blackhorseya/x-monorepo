CREATE TABLE ordering.orders (
    id          text        PRIMARY KEY,
    customer_id text        NOT NULL,
    status      text        NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);

-- total is not stored: the aggregate recomputes it from items on load.
CREATE TABLE ordering.order_items (
    order_id         text   NOT NULL REFERENCES ordering.orders (id) ON DELETE CASCADE,
    position         int    NOT NULL,
    sku              text   NOT NULL,
    quantity         int    NOT NULL,
    unit_price_minor bigint NOT NULL,
    currency         text   NOT NULL,
    PRIMARY KEY (order_id, position)
);
