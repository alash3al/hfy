-- +goose Up
-- A session is a conversation: a durable room of people and agents. Renaming
-- is metadata only. The indexes, sequences and constraints of the two tables
-- were named from the old names, so they are renamed by pattern rather than
-- guessed. Stop every hfy process before upgrading: older binaries use the
-- old names.
ALTER TABLE sessions RENAME TO conversations;
ALTER TABLE session_events RENAME TO conversation_events;
ALTER TABLE conversation_events RENAME COLUMN session_id TO conversation_id;

-- +goose StatementBegin
DO $$
DECLARE r record;
BEGIN
    -- Renaming an index also renames the constraint it backs (primary keys).
    FOR r IN SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
             WHERE i.indrelid IN ('conversations'::regclass, 'conversation_events'::regclass)
               AND c.relname LIKE '%session%' LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', r.relname, replace(r.relname, 'session', 'conversation'));
    END LOOP;
    FOR r IN SELECT conname, conrelid::regclass AS tbl FROM pg_constraint
             WHERE conrelid IN ('conversations'::regclass, 'conversation_events'::regclass)
               AND conname LIKE '%session%' LOOP
        EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', r.tbl, r.conname, replace(r.conname, 'session', 'conversation'));
    END LOOP;
    -- Embeddings tables (embeddings_<dims>, created at boot) name what they embed.
    FOR r IN SELECT relname FROM pg_class
             WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r'
               AND relname ~ '^embeddings_[0-9]+$' LOOP
        EXECUTE format('UPDATE %I SET embeddable_type = %L WHERE embeddable_type = %L', r.relname, 'conversation_event', 'session_event');
    END LOOP;
END $$;
-- +goose StatementEnd

ALTER SEQUENCE sessions_id_seq RENAME TO conversations_id_seq;
ALTER SEQUENCE session_events_id_seq RENAME TO conversation_events_id_seq;

UPDATE jobs SET concurrency_key = 'conversation:' || substr(concurrency_key, length('session:') + 1)
WHERE concurrency_key LIKE 'session:%';

-- +goose Down
UPDATE jobs SET concurrency_key = 'session:' || substr(concurrency_key, length('conversation:') + 1)
WHERE concurrency_key LIKE 'conversation:%';

ALTER SEQUENCE conversations_id_seq RENAME TO sessions_id_seq;
ALTER SEQUENCE conversation_events_id_seq RENAME TO session_events_id_seq;

-- +goose StatementBegin
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
             WHERE i.indrelid IN ('conversations'::regclass, 'conversation_events'::regclass)
               AND c.relname LIKE '%conversation%' LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', r.relname, replace(r.relname, 'conversation', 'session'));
    END LOOP;
    FOR r IN SELECT conname, conrelid::regclass AS tbl FROM pg_constraint
             WHERE conrelid IN ('conversations'::regclass, 'conversation_events'::regclass)
               AND conname LIKE '%conversation%' LOOP
        EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', r.tbl, r.conname, replace(r.conname, 'conversation', 'session'));
    END LOOP;
    FOR r IN SELECT relname FROM pg_class
             WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r'
               AND relname ~ '^embeddings_[0-9]+$' LOOP
        EXECUTE format('UPDATE %I SET embeddable_type = %L WHERE embeddable_type = %L', r.relname, 'session_event', 'conversation_event');
    END LOOP;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_events RENAME COLUMN conversation_id TO session_id;
ALTER TABLE conversation_events RENAME TO session_events;
ALTER TABLE conversations RENAME TO sessions;
