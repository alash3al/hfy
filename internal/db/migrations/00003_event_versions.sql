-- +goose Up
-- Every event records the version of its payload's shape. Rows written
-- before this existed are version 1. (A constant default is metadata-only:
-- no table rewrite.)
ALTER TABLE session_events ADD COLUMN v smallint NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE session_events DROP COLUMN v;
