-- +goose Up
-- A grant is a client's access to one conversation as one actor, created by
-- the app's backend with the server secret. Only the token's sha256 is kept.
-- Revoking sets revoked_at and pulls expires_at to now, so a grant is valid
-- exactly while expires_at is in the future.
CREATE TABLE grants (
    id              bigserial PRIMARY KEY,
    uuid            uuid        NOT NULL DEFAULT gen_random_uuid(),
    token_hash      bytea       NOT NULL,
    conversation_id bigint      NOT NULL REFERENCES conversations (id),
    actor_id        bigint      NOT NULL REFERENCES actors (id),
    scope           text        NOT NULL,
    agent           text        NOT NULL,
    vars            jsonb       NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz
);
CREATE UNIQUE INDEX ON grants (uuid);                                            -- revoke
CREATE UNIQUE INDEX ON grants (token_hash);                                      -- every client request
CREATE INDEX ON grants (expires_at);                                             -- retention

-- +goose Down
DROP TABLE grants;
