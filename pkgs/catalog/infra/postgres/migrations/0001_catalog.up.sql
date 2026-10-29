-- Products: what one SKU sells for. The price is minor units plus its own
-- ISO 4217 currency (each product has one); the checks keep the domain's
-- invariants true for a row nobody wrote through the aggregate.
CREATE TABLE catalog.products (
    sku            text        PRIMARY KEY CHECK (octet_length(sku) BETWEEN 1 AND 64),
    name           text        NOT NULL CHECK (btrim(name) <> '' AND char_length(name) <= 200),
    price_amount   bigint      NOT NULL CHECK (price_amount >= 0),
    price_currency char(3)     NOT NULL CHECK (price_currency ~ '^[A-Z]{3}$'),
    active         boolean     NOT NULL,
    -- optimistic lock: Save updates only WHERE version matches, then bumps it
    version        bigint      NOT NULL DEFAULT 0,
    updated_at     timestamptz NOT NULL
);

-- ListProducts filters by literal SKU prefix (sku LIKE 'prefix%'). The primary key btree is built
-- with the database collation, which cannot serve LIKE prefixes; text_pattern_ops compares
-- byte-wise so it can. It stays a separate index so ORDER BY sku keeps using the PK.
CREATE INDEX products_sku_prefix_idx ON catalog.products (sku text_pattern_ops);
