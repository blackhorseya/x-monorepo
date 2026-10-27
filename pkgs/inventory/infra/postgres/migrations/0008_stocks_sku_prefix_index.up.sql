-- ListStocks filters by literal SKU prefix (sku LIKE 'prefix%'). The primary key btree is built
-- with the database collation (en_US.utf8), which cannot serve LIKE prefixes; text_pattern_ops
-- compares byte-wise so it can. It stays a separate index so ORDER BY sku keeps using the PK.
CREATE INDEX stocks_sku_prefix_idx ON inventory.stocks (sku text_pattern_ops);
