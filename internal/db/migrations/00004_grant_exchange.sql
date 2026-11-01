-- +goose Up
-- A renewable grant (minted with max_lifetime) swaps its token for a fresh one
-- until renew_until; every swap is a new row of one chain, named by root_id (the
-- first row's uuid, which the backend stored). renew_secs is the lifetime each
-- new row gets. NULL renew_until: not renewable, the grant it always was.
-- All three columns are nullable and have no default, so this is a catalog
-- change only: no table rewrite, token lookups keep running. A row without a
-- root_id (made before this, or by the previous release during a rolling
-- deploy) is a chain of one, named by its own uuid; it is never renewable.
ALTER TABLE grants
    ADD COLUMN renew_until timestamptz,
    ADD COLUMN renew_secs  int,
    ADD COLUMN root_id     uuid;
CREATE INDEX ON grants (root_id);                                                -- revoke a chain

-- +goose Down
-- The previous release revokes only the row it is given, so a chain's later
-- rows would outlive their revocation: end them now. The first row of a chain
-- stays as it is, a grant that is simply no longer renewable.
UPDATE grants SET revoked_at = coalesce(revoked_at, now()), expires_at = least(expires_at, now())
WHERE root_id IS NOT NULL AND root_id <> uuid AND expires_at > now();
ALTER TABLE grants
    DROP COLUMN root_id,
    DROP COLUMN renew_secs,
    DROP COLUMN renew_until;
