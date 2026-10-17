-- Stock.Save now inserts with version 1 and updates `WHERE version = <loaded>`,
-- so a stored version counts saves (0 means "never saved" and only lives in
-- memory). Rows written by the old upsert started at 0: shift them so that a
-- fresh aggregate (version 0) can never match a stored row.
UPDATE inventory.stocks SET version = version + 1;
