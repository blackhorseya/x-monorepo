DROP TABLE identity.audit_events;
DROP TABLE identity.invites;
DROP TABLE identity.external_identities;
ALTER TABLE identity.accounts DROP COLUMN sessions_revoked_at;
