-- +goose Up
-- Every turn of a session up to settled_until has ended (turn states are
-- final), so the queries for open and queued turns start after it instead of
-- scanning the whole session. svc moves it, under the session lock, in the
-- transaction that ends turns; it only ever lags. Existing sessions start at
-- 0: correct, and settled as their turns end. (00004 is skipped on purpose: a
-- removed, never released migration may hold that number in dev databases.)
ALTER TABLE sessions ADD COLUMN settled_until bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN settled_until;
