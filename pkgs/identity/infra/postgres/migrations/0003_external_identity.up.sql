-- When the account's sessions were last revoked: an access token issued
-- before it is refused. NULL: never revoked. Nullable and defaultless, so the
-- ALTER is a catalog change, not a table rewrite.
ALTER TABLE identity.accounts ADD COLUMN sessions_revoked_at timestamptz;

-- External identities: what an OIDC issuer vouches for, (issuer, subject),
-- bound to exactly one account. The primary key is the one-to-one guarantee;
-- an account may hold several. Bound by subject, never by email.
CREATE TABLE identity.external_identities (
    issuer     text        NOT NULL,
    subject    text        NOT NULL,
    account_id text        NOT NULL REFERENCES identity.accounts (id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX external_identities_account_idx ON identity.external_identities (account_id);

-- Invites: an admin's pre-binding of an email to an account. No secret is
-- stored. claimed_at set = spent; the claim updates the row under its lock
-- (SELECT ... FOR UPDATE), so two claimants cannot both succeed.
CREATE TABLE identity.invites (
    id         text        PRIMARY KEY,
    email      text        NOT NULL,
    account_id text        NOT NULL REFERENCES identity.accounts (id),
    version    bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    claimed_at timestamptz
);
CREATE INDEX invites_email_idx ON identity.invites (email, created_at DESC, id DESC);

-- Audit: who did what to which account, when. Never a token. No foreign key:
-- the record outlives whatever it names.
CREATE TABLE identity.audit_events (
    id                bigserial   PRIMARY KEY,
    actor             text        NOT NULL,
    action            text        NOT NULL,
    target_account_id text        NOT NULL,
    occurred_at       timestamptz NOT NULL
);
