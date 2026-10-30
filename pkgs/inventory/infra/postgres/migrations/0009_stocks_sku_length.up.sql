-- Same SKU rule as catalog's products: 1 to 64 bytes.
-- A single ADD CONSTRAINT: the migration driver runs the file as one transaction, so the
-- ACCESS EXCLUSIVE lock is held to commit whether or not it is split into NOT VALID + VALIDATE.
-- Reads and writes on inventory.stocks wait while the existing rows are checked (one row per
-- SKU, so short); docs/operations.md has the details.
-- Fails if a stock row already breaks the rule; docs/operations.md has the query to run first.
ALTER TABLE inventory.stocks ADD CONSTRAINT stocks_sku_length CHECK (octet_length(sku) BETWEEN 1 AND 64);
