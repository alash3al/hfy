# hfy

A simple harness as a service. One Go binary plus Postgres, configured with one
YAML file, serving one agent over HTTP.

Your app creates sessions and posts messages. hfy runs the agent loop, calls the
tools, keeps the history and streams events back. You stop rebuilding the same
plumbing (loop, tools, history, streaming, auth hooks) in every app.

- **Small surface.** Tools come only from MCP servers, models only from
  OpenAI-compatible providers, auth only through your webhook.
- **Model-agnostic.** The model is a config line.
- **Durable.** Every turn runs from a Postgres queue and every step is an
  append-only event. Dropped connections, restarts and crashes lose nothing.
- **One instance = one domain = one tenant.** Many end users can share it
  (isolation is per session). Run another instance for another app.

## Quick start

```sh
hfy config:example > harness.yaml     # edit: provider, model, system prompt
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
export DATABASE_URL=postgres://postgres:hfy@localhost:5432/hfy   # pgvector 0.8.0+
hfy validate
hfy serve --work                      # API + workers; migrations run on boot
```

Or `docker compose up` (see `compose.yaml`; it mounts `./harness.yaml`).

```sh
# your backend: create a session and hand its id to the client
curl -XPOST :8080/v1/sessions -H "Authorization: Bearer $HFY_SERVER_SECRET"
# {"id":"7f3c…"}

# the client: open the stream, then post a message
curl -N :8080/v1/sessions/7f3c…/events
curl -XPOST :8080/v1/sessions/7f3c…/messages -d '{"text":"hello","vars":{"user_name":"Ann"}}'
# 202 {"turn":"a91e…"}
```

## Commands

| Command | |
|---|---|
| `hfy -c harness.yaml serve [--work]` | HTTP API and SSE; `--work` adds workers in the same process |
| `hfy queue:work [-q turns,embeddings] [--workers N]` | workers only; `-q` is a priority-ordered list (default: all) |
| `hfy validate` | check config, env expansion and startup rules |
| `hfy migrate [up\|down\|status]` | goose migrations (`serve` and `queue:work` also run `up` on boot) |
| `hfy config:example` | print the commented starter config |
| `hfy version` | |

`-c` also reads `HFY_CONFIG`. API and workers scale separately: workers append
events and `NOTIFY`; every `serve` instance pushes them to its own SSE clients.

## Config

`hfy config:example` is the reference. In short: `server`, `storage`, `providers`,
`mcp`, `agent`, plus optional `auth`, `queue`, `retention`, `embeddings`,
`attachments`, `subagents`, `summarizer`, `metrics`.

- Unknown keys stop startup with their line (`line 12: unknown key "aproval"
  in config.agent`): a typo never silently disables a setting.

- `${VAR}` / `${VAR:-default}` expand from the environment in string values
  (never keys); a missing variable without a default stops startup and names
  it; `$$` is a literal `$`. Quote the value inside YAML flow maps (`{ a: "${X}" }`).
- `agent.settings.context_window` is required (there is no auto-discovery).
  Set it somewhat below the model's real limit.
- `agent.mcp` omitted = all MCP servers, reached through `harness__tool_search`
  (small prompt). Set it to restrict, and the tools are listed directly.

## API

The API is versioned in the path (`/v1`). Within `v1`, changes are additive
only: new endpoints, new optional fields, new event types. A breaking change
would come as `/v2`, served alongside `/v1`.

Server to server (`Authorization: Bearer <server.secret>`):

- `POST /v1/sessions` → `201 {"id": uuid}`

Client (`Authorization: Bearer <key>`, or `?key=` for `EventSource`). With
`auth` configured, hfy sends `POST {key, session_id}` to your webhook and
expects `200 {actor, scope?, vars?}`; `401`/`403`, any other status, an error
or a timeout denies. Without `auth` the caller is `anonymous` and unguessable
session ids are the only protection.

- `POST /v1/sessions/{id}/messages` `{text, attachments?, vars?}` → `202 {"turn": uuid}`.
  Starts the turn, or queues it behind the active one.
- `GET /v1/sessions/{id}/events` SSE. `id` = event uuid, `event` = type, `data` = envelope.
  Resume with `Last-Event-ID`, or `?after=<event uuid>` on a first connection.
  Without either it starts from now. A `: ping` comment arrives every 15 s.
- `GET /v1/sessions/{id}/history?limit=&before=` durable events grouped by turn,
  newest page first, keyset-paginated by turn uuid. Returns `next` (cursor for
  older turns) and `live_after` (open `/events?after=<live_after>` to continue
  without a gap).
- `POST /v1/sessions/{id}/approvals` `{call_id, approve, result?}`
- `POST /v1/sessions/{id}/stop` stops the active turn and cancels queued ones.
Errors are `{"error": "..."}`: `400` bad input (unknown or missing vars,
bad attachment), `401`/`403` auth, `404` unknown session, `409` no pending
approval.

### Operations

- `GET /healthz` liveness: the process answers (never checks dependencies).
- `GET /readyz` readiness: Postgres is reachable (`503` otherwise).
- `metrics: {listen: "127.0.0.1:9090"}` opens an operational listener in every
  process (`serve` and `queue:work`) with `/metrics` (Prometheus), `/healthz`
  and `/readyz`. It is separate from the public API port.

| Metric | Labels |
|---|---|
| `hfy_http_requests_total`, `hfy_http_request_duration_seconds` | `route` (pattern), `method`, `code` |
| `hfy_event_streams` | open SSE streams on the instance |
| `hfy_turns_total` | `state` (final) |
| `hfy_tokens_total` | `agent`, `kind` (`input` `output` `cached`) |
| `hfy_provider_requests_total`, `hfy_provider_request_duration_seconds` | `provider`, `kind` (`chat` `embedding`), `outcome` |
| `hfy_tool_calls_total`, `hfy_tool_call_duration_seconds` | `server`, `outcome` |
| `hfy_jobs_total`, `hfy_job_duration_seconds` | `type`, `outcome` (`done` `retry` `failed` `lost` `released` `expired`) |
| `hfy_jobs` | `queue`, `state` (`queued` `running`), across all instances |

Plus the Go runtime and process collectors.

### Events

One envelope for the stream and for history:

```json
{"id":"…","type":"tool_call","parent":"<turn uuid>","actor":{"uuid":"…","type":"agent","ref":"main"},
 "v":1,"data":{"name":"files.read","call_id":"c1","arguments":{}},"created_at":"…"}
```

`turn` (root; `parent` is null) · `turn.state` (`queued` `running`
`awaiting_approval` `completed` `failed` `stopped` `cancelled`) ·
`turn.cancel_requested` · `message.text` · `reasoning` · `tool_call` ·
`tool_call.interrupted` · `tool_result` · `approval.requested` ·
`approval.decided` · `model.usage` · `summary` · ephemeral: `text.delta`,
`reasoning.delta` (never in history).

Render text by appending `text.delta` under its turn; the durable
`message.text` replaces them. Pair `tool_call` and `tool_result` by
`data.call_id`. Ignore event types you don't know.

`v` is the version of `data`'s shape for that type. Additive changes (a new
optional field) keep the version; a breaking change bumps it. Events are
stored forever and never rewritten: hfy upgrades older versions to the
current shape when it reads them, so clients only ever see the current `v`.
The exception is a rolling deploy, where an older instance may serve a newer
event as stored.

## How it behaves

- **Turns** run one at a time per session. Messages posted meanwhile queue.
  A turn paused for approval keeps the session blocked.
- **Crash recovery.** An expired lease re-runs the turn from its events. A tool
  call in flight when a worker died is never re-run: it gets
  `tool_call.interrupted` and the model is told. A worker that lost its lease
  (stalled, then reaped) is cancelled at its next heartbeat and can no longer
  complete or retry the job. Graceful shutdown hands running jobs straight back.
- **Loop rules.** Tool errors, invalid arguments and unknown tools come back to
  the model as error results (an unknown tool lists the valid names). The same
  call a third time is refused with advice; a sixth ends the turn. Model calls
  retry on 429, 5xx and network errors up to `queue.max_attempts`. A provider's
  `timeout` is the longest silence allowed (before the answer starts, then
  between streamed chunks), so long answers are never cut off. `limits`
  (`steps`, `total_tokens`, `timeout`) end the turn when hit.
- **Approvals.** Calls matching `agent.approval.tools` (`<server>.<tool>`
  patterns, `harness.*` works) pause the turn; nothing waits in a worker. After
  `approval.timeout` the call is rejected. Approval covers the main agent's
  calls only: a sub-agent's own tool calls are not checked, so if a sub-agent
  can reach a guarded tool, guard the sub-agent itself (`harness.<name>`).
- **Sub-agents** are tools (`harness__<name>`), isolated (task text only), depth 1;
  their usage counts toward the parent's turn.
- **Memory.** The summarizer appends a `summary` event when the prompt reaches
  `context_window` (history is never deleted). With `embeddings` configured,
  `harness__session_search` finds earlier messages of the same `scope`.
- **Attachments** are references your app hosts. Hosts, types and size are
  checked when the message is posted. Images go to the model only in the turn
  they arrive in; local providers get the bytes inlined.
- **Retention.** Ephemeral events, `outbound_requests` and finished jobs expire
  after `retention.trace`; durable events and usage are permanent.
- Tool names reach the provider as `<server>__<tool>` (`files.read` →
  `files__read`); events and approval patterns use `files.read`.

## Development

```sh
docker run -d -p 127.0.0.1:54330:5432 -e POSTGRES_PASSWORD=hfy -e POSTGRES_DB=hfy pgvector/pgvector:pg17
golangci-lint run ./... && go test -race ./...   # exactly what CI runs
```

Tests need a Postgres with pgvector (`HFY_TEST_DB` overrides
`postgres://postgres:hfy@127.0.0.1:54330/hfy`); each test gets its own
database. Locally they skip without one; CI sets `HFY_TEST_REQUIRE_DB=1` so a
missing database fails instead. A scriptable fake OpenAI server
(`internal/provider/fakeprov`) and a fake stdio MCP server
(`internal/mcp/fakemcp`) stand in for real ones.

**Toolchain.** `go.mod` pins Go 1.27.3 and the `go` command switches to it,
but tools built with an older Go reject newer syntax (generic methods) with
"method must have no type parameters". Build them with the module's toolchain,
and format with `go fmt` or `golangci-lint fmt`, not a `gofmt` from `PATH`:

```sh
GOTOOLCHAIN=go1.27.3 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOTOOLCHAIN=go1.27.3 go install golang.org/x/tools/gopls@latest
```

Layout: `internal/{config,types,db,jobs,agent,mcp,httpapi,auth,svc,provider,metrics}`;
migrations, SQL queries, prompts and the example config are embedded in the
binary. `hfy.New(cfg).Run(ctx)` embeds the harness in another Go program.

### Conventions

Each rule says what enforces it. A rule that only lives in prose does not
survive many contributors, so prefer adding a check to adding a sentence.

| Rule | Enforced by |
|---|---|
| **Layering.** Imports follow the table in `arch_test.go`; `httpapi` never imports `agent` (API and workers scale apart), `types` imports nothing. | `TestLayering` |
| **One transaction, one connection.** A function holding a `pgx.Tx` never touches `.Pool`: a second connection deadlocks once the pool runs out. | `TestTransactionsUseOnlyTheirConnection`, `TestMutationsNeedOneConnection` |
| **`svc` owns turn state.** Every `turn.state` change is a `svc` method under the session lock; the agent appends only what it produces. | `TestOnlySvcWritesTurnState` |
| **The event log is the truth.** State is folded from events (`svc.ViewOf`, `agent.BuildContext`); events are never updated; payloads are read with `e.Payload[T]()`, and one that does not decode is an error. | review; `Payload` is the only decoder |
| **Stored data is versioned.** Payloads carry `v` and are upgraded on read (`types.register`); SQL-read fields are frozen. A breaking job payload change is a new job type. Migrations stay compatible with the previous release (add first, remove a release later). | `TestEventVersions`, `TestClaimOnlyHandledTypes`, review |
| **Writes that belong together commit together** (an event and the job it implies). | review |
| **Jobs are fenced by attempt** and resume from the log; side effects are never repeated blindly. | `TestStaleWorkerCannotTouchAReclaimedJob`, crash-recovery tests |
| **No silent failure.** Errors are returned, or logged with session and turn when nothing can act on them. | `errcheck`; review |
| **Current Go.** `slices`/`maps`, range-over-int, `new(expr)`, `t.Context()`, generic methods. | `modernize` |
| **Every `//nolint` names its linter and says why.** | `nolintlint` |

### Departures from the design handoff

- No `Router.On[T]`: the few places that fold events use a plain `switch` on
  the type, which is less code and easier to follow.
- The `LISTEN` connection is opened separately instead of being held out of the
  pool (same effect, simpler lifecycle).
- Decisions the handoff left open: `embeddings:` is optional; sub-agent steps
  are not stored as events (their `model.usage` and `outbound_requests` are);
  attachments on later turns are a placeholder; no secret rotation.

### Not in v1

Images returned by MCP tools (they become a text placeholder), an auth cache,
semantic `tool_search` (keyword today), structured output, provider failover,
other stream formats, a JS client, an OpenAI-compatible adapter.
