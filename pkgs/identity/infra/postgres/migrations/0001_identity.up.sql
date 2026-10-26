-- Accounts: who an API key speaks for. roles is the list RequireRole checks,
-- a JSON array of strings (jsonb: no driver-specific array type needed).
CREATE TABLE identity.accounts (
    id         text        PRIMARY KEY,
    name       text        NOT NULL,
    roles      jsonb       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(roles) = 'array'),
    status     text        NOT NULL,
    -- optimistic lock: Save updates only WHERE version matches, then bumps it
    version    bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

-- API keys: only the SHA-256 of the secret is stored; the auth path looks a
-- presented key up by that hash (unique). prefix is the clear leading part
-- for listings. Revoked keys stay (who had what, until when).
CREATE TABLE identity.api_keys (
    id         text        PRIMARY KEY,
    account_id text        NOT NULL REFERENCES identity.accounts (id),
    name       text        NOT NULL,
    prefix     text        NOT NULL,
    hash       bytea       NOT NULL UNIQUE CHECK (octet_length(hash) = 32),
    status     text        NOT NULL,
    version    bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX api_keys_account_idx ON identity.api_keys (account_id, created_at DESC, id DESC);
