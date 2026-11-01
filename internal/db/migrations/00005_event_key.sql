-- +goose NO TRANSACTION
-- +goose Up
-- An event may carry a key, unique within (conversation, actor): a host that
-- sends the same event twice gets the first one back. Events without a key, all
-- of them today, are not in the index.
--
-- conversation_events is the hottest table, and a plain CREATE INDEX holds a
-- lock that blocks every insert for as long as it reads the table (even for a
-- partial index that ends up empty), so this runs outside a transaction and
-- builds the index CONCURRENTLY: writers keep going. goose runs each statement
-- by itself here, so the lock_timeout of the ALTER is a DO block: SET LOCAL
-- lasts for that one statement.
--
-- A build that fails (or is cancelled) leaves an INVALID index of this name
-- behind, which CREATE INDEX IF NOT EXISTS would then skip: the DROP below makes
-- running the migration again safe. A concurrent build also waits for every
-- transaction that started before it, so a transaction left idle holds it up.
--
-- +goose StatementBegin
DO $$
BEGIN
    SET LOCAL lock_timeout = '5s';
    ALTER TABLE conversation_events ADD COLUMN IF NOT EXISTS key text;
END $$;
-- +goose StatementEnd
DROP INDEX CONCURRENTLY IF EXISTS conversation_events_key_idx;
CREATE UNIQUE INDEX CONCURRENTLY conversation_events_key_idx
    ON conversation_events (conversation_id, actor_id, key) WHERE key IS NOT NULL;

-- +goose Down
-- Events keep their rows; they only lose the key they were sent under.
DROP INDEX CONCURRENTLY IF EXISTS conversation_events_key_idx;
-- +goose StatementBegin
DO $$
BEGIN
    SET LOCAL lock_timeout = '5s';
    ALTER TABLE conversation_events DROP COLUMN IF EXISTS key;
END $$;
-- +goose StatementEnd
