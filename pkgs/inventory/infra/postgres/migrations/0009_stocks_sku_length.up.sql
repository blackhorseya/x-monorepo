-- Same SKU rule as catalog's products: 1 to 64 bytes. Added NOT VALID (no table scan under the
-- ACCESS EXCLUSIVE lock), then validated (SHARE UPDATE EXCLUSIVE: reads and writes continue).
-- Fails if a stock row already breaks the rule; docs/operations.md has the query to run first.
ALTER TABLE inventory.stocks ADD CONSTRAINT stocks_sku_length CHECK (octet_length(sku) BETWEEN 1 AND 64) NOT VALID;
ALTER TABLE inventory.stocks VALIDATE CONSTRAINT stocks_sku_length;
