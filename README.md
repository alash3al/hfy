# hfy

A simple harness as a service. One Go binary plus Postgres, configured with one
YAML file, serving your agents over HTTP.

Your app creates conversations and posts messages. hfy runs the agent loop, calls the
tools, keeps the history and streams events back. You stop rebuilding the same
plumbing (loop, tools, history, streaming, auth hooks) in every app.

- **Small surface.** Tools come only from MCP servers, models only from
  OpenAI-compatible providers, auth only through your webhook.
- **Model-agnostic.** The model is a config line.
- **Durable.** Every turn runs from a Postgres queue and every step is an
  append-only event. Dropped connections, restarts and crashes lose nothing.
- **One instance = one domain = one tenant.** Many end users can share it
  (isolation is per conversation). Run another instance for another app.

## Quick start

```sh
hfy config:example > hfy.yaml     # edit: provider, model, system prompt
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
export DATABASE_URL=postgres://postgres:hfy@localhost:5432/hfy   # pgvector 0.8.0+
hfy validate
hfy serve --work                      # API + workers; migrations run on boot
```

Or `docker compose up` (see `compose.yaml`; it mounts `./hfy.yaml`).

```sh
# your backend: create a conversation and hand its id to the client
curl -XPOST :8080/v1/conversations -H "Authorization: Bearer $HFY_SERVER_SECRET"
# {"id":"7f3c…"}

# the client: open the stream, then post a message
curl -N :8080/v1/conversations/7f3c…/events
curl -XPOST :8080/v1/conversations/7f3c…/messages -d '{"text":"hello"}'
# 202 {"turn":"a91e…"}
```

## Commands

| Command | |
|---|---|
| `hfy -c hfy.yaml serve [--work]` | HTTP API and SSE; `--work` adds workers in the same process |
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
- `agent.settings.context_window` is required (there is no auto-discovery):
  the model's real window. History is compacted at 80% of it, leaving room
  for the reply and the step's tool results.
  Set it somewhat below the model's real limit.
- `agent.mcp` omitted = all MCP servers, reached through `hfy__tool_search`
  (small prompt). Set it to restrict, and the tools are listed directly.

## API

The API is versioned in the path (`/v1`). Within `v1`, changes are additive
only: new endpoints, new optional fields, new event types. A breaking change
would come as `/v2`, served alongside `/v1`.

Server to server (`Authorization: Bearer <server.secret>`):

- `POST /v1/conversations` → `201 {"id": uuid}`

Client (`Authorization: Bearer <key>`, or `?key=` for `EventSource`). With
`auth` configured, hfy sends `POST {key, conversation_id, hint?}` to your webhook and
expects `200 {actor, scope?, vars?, agent?}`; `401`/`403`, any other status, an
error or a timeout denies. `agent` names the configured agent that serves this
caller (`agents.<name>`; default `main`), so one deployment can run, say, a
store agent for buyers and an admin agent for staff; an unknown name denies.
`hint` is the client's `Hfy-Hint` header: any JSON (at most 4 KiB), say
`{"agent": "store"}` when posting a message, that your webhook may honor or
ignore. hfy forwards it unread and never trusts or stores it. `vars` fill the
agent's declared `{{vars}}`: they render into the system prompt, so only the
webhook sets them, never a client (undeclared ones are dropped; a missing
required one is a server error, the webhook's). Without `auth` the caller is
`anonymous`, vars take their defaults, and unguessable conversation ids are the
only protection.

- `POST /v1/conversations/{id}/messages` `{text, attachments?}` → `202 {"turn": uuid}`.
  Starts the turn, or queues it behind the active one.
- `GET /v1/conversations/{id}/events` SSE. `id` = event uuid, `event` = type, `data` = envelope.
  Resume with `Last-Event-ID`, or `?after=<event uuid>` on a first connection.
  Without either it starts from now. A `: ping` comment arrives every 15 s.
- `GET /v1/conversations/{id}/history?limit=&before=` durable events grouped by turn,
  newest page first, keyset-paginated by turn uuid. Returns `next` (cursor for
  older turns) and `live_after` (open `/events?after=<live_after>` to continue
  without a gap).
- `POST /v1/conversations/{id}/approvals` `{call_id, approve, result?}`. Only the
  actor who started the turn decides its calls (`403` otherwise).
- `POST /v1/conversations/{id}/stop` stops the caller's own turns: the active one
  if they started it, and their queued ones. Everyone else's keep their place.
Errors are `{"error": "..."}`: `400` bad input (an empty message, a bad
attachment, a body over 2 MiB), `401`/`403` auth (or not your turn), `404`
unknown conversation, `409` no pending approval.

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
`data.call_id`; a `tool_result` may carry `data.structured` (the tool's MCP
`structuredContent`, e.g. a UI component to render). The agent behind a turn
is `turn.data.agent`, and its events' `actor.ref`. Ignore event types you
don't know.

`v` is the version of `data`'s shape for that type. Additive changes (a new
optional field) keep the version; a breaking change bumps it. Events are
stored forever and never rewritten: hfy upgrades older versions to the
current shape when it reads them, so clients only ever see the current `v`.
The exception is a rolling deploy, where an older instance may serve a newer
event as stored.

## How it behaves

- **Turns** run one at a time per conversation. Messages posted meanwhile queue.
  A turn paused for approval keeps the conversation blocked.
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
  patterns, `hfy.*` works) pause the turn; nothing waits in a worker. After
  `approval.timeout` the call is rejected. Each agent has its own `approval`.
  Approval covers an agent's own calls only: a sub-agent's own tool calls are not checked, so if a sub-agent
  can reach a guarded tool, guard the sub-agent itself (`hfy.<name>`).
- **Core tools.** Every agent gets hfy's own tools, served by the built-in `hfy`
  server (the name is reserved): `hfy.now {timezone?}` (UTC, and local time,
  weekday and offset for an IANA zone) and `hfy.math_calc {expression}`
  (arithmetic, comparisons and math functions via
  [expr](https://github.com/expr-lang/expr), math built-ins only; numbers
  never wrap around and results drop float noise). A tool is core only if it
  is pure (no I/O, no app data, no permissions) and does what models get
  wrong alone; anything touching data belongs to your MCP servers.
- **Sub-agents** are tools (`hfy__<name>`), isolated (task text only), depth 1;
  their usage counts toward the parent's turn. Every agent may call them.
- **Tool servers know who is calling.** Every MCP tool call carries `_meta`
  `"io.github.alash3al.hfy/turn": {conversation, turn, call_id, agent, actor: {type,
  ref}, scope, vars}`, so your MCP server can authorize each call itself (the
  actor is whoever started the turn; `vars` are the webhook's, so keep
  secrets out of them: every MCP server, remote ones too, receives them). Return
  `structuredContent` for data meant for the client; the model sees the text.
- **A conversation is a room.** Every agent that answers in it sees the whole
  conversation: its own messages and tool calls as its own, and, once others
  speak (another agent, or a second person), everyone else's messages framed
  with who wrote them: `<message from="u1" kind="person">…</message>`. The
  text inside is escaped, so nobody can forge a frame, and the system prompt
  explains them. Other agents' tool calls stay theirs. A one-to-one chat is
  sent plain. Frames exist only in the prompt (stored events and the API are
  unchanged); make actor refs readable (the webhook's `actor`) if the model
  should know names. Frames tell the model who spoke; they are not
  authorization: tools decide from `_meta`, which the model cannot change.
- **Memory.** The summarizer appends a `summary` event when the next prompt
  would pass 80% of `context_window` (history is never deleted). It folds
  earlier turns, never the running one, so a single turn with very large tool
  results can still outgrow the window. With `embeddings` configured,
  `hfy__conversation_search` finds earlier messages of the same `scope`.
- **MCP servers.** A local server inherits only `PATH`, `HOME`, `USER`, locale,
  `TMPDIR` and `TZ` from hfy, never its secrets; give it anything else in
  `environment`. Each tool call has `timeout` (default 2m) to answer.
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
| **`svc` owns turn state.** Every `turn.state` change is a `svc` method under the conversation lock; the agent appends only what it produces. | `TestOnlySvcWritesTurnState` |
| **The event log is the truth.** State is folded from events (`svc.ViewOf`, `agent.BuildContext`); events are never updated; payloads are read with `e.Payload[T]()`, and one that does not decode is an error. | review; `Payload` is the only decoder |
| **Stored data is versioned.** Payloads carry `v` and are upgraded on read (`types.register`); SQL-read fields are frozen. A breaking job payload change is a new job type. Migrations stay compatible with the previous release (add first, remove a release later). | `TestEventVersions`, `TestClaimOnlyHandledTypes`, review |
| **Writes that belong together commit together** (an event and the job it implies). | review |
| **Jobs are fenced by attempt** and resume from the log; side effects are never repeated blindly. | `TestStaleWorkerCannotTouchAReclaimedJob`, crash-recovery tests |
| **No silent failure.** Errors are returned, or logged with conversation and turn when nothing can act on them. | `errcheck`; review |
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
