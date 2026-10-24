-- +goose Up
-- conversation_files is a queryable index of every file attached to a
-- conversation message. The event log stays the truth (message.text keeps
-- {file, mime, name}); rows are written in the message's own transaction and
-- are only ever inserted, never updated. No row exists for an abandoned
-- upload: only a verified copy of a posted message lands here.
CREATE TABLE conversation_files (
    id              bigserial PRIMARY KEY,
    uuid            uuid        NOT NULL DEFAULT gen_random_uuid(),
    conversation_id bigint      NOT NULL REFERENCES conversations (id),
    event_id        bigint      NOT NULL REFERENCES conversation_events (id), -- the message.text
    actor_id        bigint      NOT NULL REFERENCES actors (id),              -- who uploaded
    key             text        NOT NULL, -- conversations/<conv>/<rand><ext>; never a URL or bucket
    mime            text        NOT NULL,
    size            bigint      NOT NULL,
    name            text        NOT NULL DEFAULT '', -- original filename, display only, untrusted
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (size > 0)
);
CREATE UNIQUE INDEX ON conversation_files (uuid);
CREATE UNIQUE INDEX ON conversation_files (key);
CREATE INDEX ON conversation_files (conversation_id, id);
CREATE INDEX ON conversation_files (event_id);
-- +goose Down
DROP TABLE conversation_files;
