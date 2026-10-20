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

CREATE TABLE sessions (
    id             bigserial PRIMARY KEY,
    uuid           uuid        NOT NULL DEFAULT gen_random_uuid(),
    active_turn_id bigint,
    summary_id     bigint,
    input_tokens   bigint      NOT NULL DEFAULT 0,
    output_tokens  bigint      NOT NULL DEFAULT 0,
    cached_tokens  bigint      NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON sessions (uuid);                                          -- session by public id

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

CREATE TABLE session_events (
    id         bigserial PRIMARY KEY,
    uuid       uuid        NOT NULL DEFAULT gen_random_uuid(),
    session_id bigint      NOT NULL REFERENCES sessions (id),
    parent_id  bigint      REFERENCES session_events (id),
    actor_id   bigint      NOT NULL REFERENCES actors (id),
    scope      text        NOT NULL,
    type       text        NOT NULL,
    data       jsonb       NOT NULL DEFAULT '{}',
    ephemeral  boolean     NOT NULL DEFAULT false,
    request_id bigint,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON session_events (uuid);                                    -- Last-Event-ID, ?after=, cursors
CREATE INDEX ON session_events (session_id, id);                                 -- live stream replay (all events)
CREATE INDEX ON session_events (session_id, id) WHERE NOT ephemeral;             -- context, history (hot, smaller)
CREATE INDEX ON session_events (session_id, id) WHERE type = 'turn';             -- history paginated by turn
CREATE INDEX ON session_events (parent_id, id) WHERE parent_id IS NOT NULL;      -- events of one turn
CREATE INDEX ON session_events (created_at) WHERE ephemeral;                     -- retention

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
CREATE INDEX ON jobs (concurrency_key) WHERE state = 'queued';                   -- stop: cancel a session's queued turns
CREATE INDEX ON jobs (lease_until) WHERE state = 'running';                      -- expired leases
CREATE INDEX ON jobs (created_at) WHERE state IN ('done', 'failed');             -- pruning finished jobs

-- +goose Down
DROP TABLE jobs;
DROP TABLE session_events;
DROP TABLE outbound_requests;
DROP TABLE sessions;
DROP TABLE actors;
