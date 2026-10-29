-- When the key last resolved, to the minute (the resolver throttles the write,
-- auth.last_used_interval). NULL: never used. Nullable and defaultless, so
-- the ALTER is a catalog change, not a table rewrite. Not part of the
-- optimistic lock: a touch never bumps version.
ALTER TABLE identity.api_keys ADD COLUMN last_used_at timestamptz;
