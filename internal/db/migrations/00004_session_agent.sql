-- +goose Up
-- A session is one room: the agent that answers in it is chosen when it is
-- created and never changes. Sessions created before this are main's.
ALTER TABLE sessions ADD COLUMN agent text NOT NULL DEFAULT 'main';

-- +goose Down
ALTER TABLE sessions DROP COLUMN agent;
