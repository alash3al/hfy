-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE actors (
    id         bigserial PRIMARY KEY,
    uuid       uuid        NOT NULL DEFAULT gen_random_uuid(),
    type       text        NOT NULL,
    ref        text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON actors (uuid);
CREATE UNIQUE INDEX ON actors (type, ref);                                       -- find-or-create

-- Every turn of a conversation up to settled_until has ended (turn states are
-- final), so the queries for open and queued turns start after it instead of
-- scanning the whole conversation. svc moves it, under the conversation lock,
-- in the transaction that ends turns; it only ever lags.
CREATE TABLE conversations (
    id             bigserial PRIMARY KEY,
    uuid           uuid        NOT NULL DEFAULT gen_random_uuid(),
    active_turn_id bigint,
    summary_id     bigint,
    settled_until  bigint      NOT NULL DEFAULT 0,
    input_tokens   bigint      NOT NULL DEFAULT 0,
    output_tokens  bigint      NOT NULL DEFAULT 0,
    cached_tokens  bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON conversations (uuid);                                     -- conversation by public id

CREATE TABLE outbound_requests (
    id            bigserial PRIMARY KEY,
    turn_event_id bigint,
    kind          text        NOT NULL,
    url           text        NOT NULL,
    request       jsonb,
    response      jsonb,
    status_code   int,
    latency_ms    int,
    attempt       int         NOT NULL DEFAULT 1,
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON outbound_requests (turn_event_id);                               -- trace of a turn
CREATE INDEX ON outbound_requests (created_at);                                  -- retention

-- v is the version of the payload's shape.
CREATE TABLE conversation_events (
    id              bigserial PRIMARY KEY,
    uuid            uuid        NOT NULL DEFAULT gen_random_uuid(),
    conversation_id bigint      NOT NULL REFERENCES conversations (id),
    parent_id       bigint      REFERENCES conversation_events (id),
    actor_id        bigint      NOT NULL REFERENCES actors (id),
    scope           text        NOT NULL,
    type            text        NOT NULL,
    v               smallint    NOT NULL DEFAULT 1,
    data            jsonb       NOT NULL DEFAULT '{}',
    ephemeral       boolean     NOT NULL DEFAULT false,
    request_id      bigint,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON conversation_events (uuid);                               -- Last-Event-ID, ?after=, cursors
CREATE INDEX ON conversation_events (conversation_id, id);                       -- live stream replay (all events)
CREATE INDEX ON conversation_events (conversation_id, id) WHERE NOT ephemeral;   -- context, history (hot, smaller)
CREATE INDEX ON conversation_events (conversation_id, id) WHERE type = 'turn';   -- history paginated by turn
CREATE INDEX ON conversation_events (parent_id, id) WHERE parent_id IS NOT NULL; -- events of one turn
CREATE INDEX ON conversation_events (created_at) WHERE ephemeral;                -- retention
CREATE INDEX conversation_events_message_text_idx
    ON conversation_events (id) WHERE type = 'message.text';                     -- embeddings backfill

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

CREATE TABLE jobs (
    id              bigserial PRIMARY KEY,
    type            text        NOT NULL,
    queue           text        NOT NULL,
    payload         jsonb       NOT NULL DEFAULT '{}',
    state           text        NOT NULL DEFAULT 'queued',
    run_at          timestamptz NOT NULL DEFAULT now(),
    attempts        int         NOT NULL DEFAULT 0,
    max_attempts    int         NOT NULL DEFAULT 3,
    lease_until     timestamptz,
    concurrency_key text,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON jobs (queue, run_at, id) WHERE state = 'queued';                 -- claim, per queue
CREATE UNIQUE INDEX ON jobs (concurrency_key) WHERE state = 'running';           -- one running job per key
CREATE INDEX ON jobs (concurrency_key) WHERE state = 'queued';                   -- stop: cancel a conversation's queued turns
CREATE INDEX ON jobs (lease_until) WHERE state = 'running';                      -- expired leases
CREATE INDEX ON jobs (created_at) WHERE state IN ('done', 'failed');             -- pruning finished jobs

-- +goose Down
DROP TABLE jobs;
DROP TABLE grants;
DROP TABLE conversation_events;
DROP TABLE outbound_requests;
DROP TABLE conversations;
DROP TABLE actors;
