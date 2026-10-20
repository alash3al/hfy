-- +goose NO TRANSACTION
-- +goose Up
-- The embeddings backfill walks message.text events by id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS session_events_message_text_idx
    ON session_events (id) WHERE type = 'message.text';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS session_events_message_text_idx;
