-- +goose Up
-- hfy no longer embeds messages. Drop the per-dimension tables an earlier
-- release made (embeddings_<dims>), matching the whole name so a table of
-- anyone else's (embeddings_x, my_embeddings_3) is never touched. The vector
-- extension stays: dropping it is the operator's call, other schemas may use it.
-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOR t IN
        SELECT tablename FROM pg_tables
        WHERE schemaname = current_schema() AND tablename ~ '^embeddings_[0-9]+$'
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS %I', t);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- No hfy of this version claims embed_events, so unfinished jobs would sit
-- forever. A running one belongs to an older worker mid-rolling-deploy; its
-- table is gone, and its later complete/retry no-ops (the lease guard finds no row).
DELETE FROM jobs WHERE type = 'embed_events' AND state IN ('queued', 'running');

-- Only the embeddings backfill read message.text events by id.
DROP INDEX IF EXISTS conversation_events_message_text_idx;

-- +goose Down
-- Dropped embeddings cannot come back; v1 rebuilds nothing on its own either.
SELECT 1;
