-- +goose Up
-- A renewable grant (minted with max_lifetime) swaps its token for a fresh one
-- until renew_until; every swap is a new row of one chain, named by root_id (the
-- first row's uuid, which the backend stored). renew_secs is the lifetime each
-- new row gets. NULL renew_until: not renewable, the grant it always was.
-- root_id has a default so the previous release, which does not know the column,
-- can still insert during a rolling deploy: its row is a chain of one.
ALTER TABLE grants
    ADD COLUMN renew_until timestamptz,
    ADD COLUMN renew_secs  int,
    ADD COLUMN root_id     uuid NOT NULL DEFAULT gen_random_uuid();
UPDATE grants SET root_id = uuid;
CREATE INDEX ON grants (root_id);                                                -- revoke a chain

-- +goose Down
ALTER TABLE grants
    DROP COLUMN root_id,
    DROP COLUMN renew_secs,
    DROP COLUMN renew_until;
