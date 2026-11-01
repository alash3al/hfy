# hfy reference

Every setting, endpoint, event and behavior. New here? Start with the [README](../README.md).

A simple harness as a service. One Go binary plus Postgres, configured with one
YAML file, serving your agents over HTTP.

Your app creates conversations and posts messages. hfy runs the agent loop, calls the
tools, keeps the history and streams events back. You stop rebuilding the same
plumbing (loop, tools, history, streaming, access) in every app.

- **Small surface.** Tools come only from MCP servers, models only from
  OpenAI-compatible providers, access only through grants your backend creates.
- **Model-agnostic.** The model is a config line.
- **Durable.** Every turn runs from a Postgres queue and every step is an
  append-only event. Dropped connections, restarts and crashes lose nothing.
- **One instance = one domain = one tenant.** Many end users can share it
  (isolation is per conversation). Run another instance for another app.

## Quick start

```sh
hfy config:example > hfy.yaml     # edit: provider, model, system prompt
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
export DATABASE_URL=postgres://postgres:hfy@localhost:5432/hfy
hfy validate
hfy serve --work                      # API + workers; migrations run on boot
```

Or `docker compose up` (see `compose.yaml`; it mounts `./hfy.yaml`).

```sh
# your backend: create a conversation, then a grant for one person in it
curl -XPOST localhost:8080/v1/conversations -H "Authorization: Bearer $HFY_SERVER_SECRET"
# {"id":"7f3c…"}
curl -XPOST localhost:8080/v1/grants -H "Authorization: Bearer $HFY_SERVER_SECRET" \
  -d '{"conversation":"7f3c…","actor":"alice"}'
# {"id":"0b8d…","token":"hfy_Qm9…","expires_at":"…"}

# the client, with only the token: open the stream, then post a message
curl -N localhost:8080/v1/conversation/events -H "Authorization: Bearer hfy_Qm9…"
curl -XPOST localhost:8080/v1/conversation/messages -H "Authorization: Bearer hfy_Qm9…" -d '{"text":"hello"}'
# 202 {"turn":"a91e…"}
```

## Commands

| Command | |
|---|---|
| `hfy -c hfy.yaml serve [--work]` | HTTP API and SSE; `--work` adds workers in the same process |
| `hfy queue:work [-q turns,maintenance] [--workers N]` | workers only; `-q` is a priority-ordered list (default: all) |
| `hfy validate` | check config, env expansion and startup rules |
| `hfy migrate [up\|down\|status]` | goose migrations (`serve` and `queue:work` also run `up` on boot) |
| `hfy config:example` | print the commented starter config |
| `hfy version` | |

`-c` also reads `HFY_CONFIG`. API and workers scale separately: workers append
events and `NOTIFY`; every `serve` instance pushes them to its own SSE clients.

## Config

`hfy config:example` is the reference. In short: `server`, `database`, `providers`,
`mcp`, `agents` (with the required `main` agent), plus optional `queue`, `retention`,
`files`, `subagents`, `summarizer`, `metrics`, `embed`.

- Unknown keys stop startup with their line (`line 12: unknown key "aproval"
  in config.agents.main`): a typo never silently disables a setting.

- `${VAR}` / `${VAR:-default}` expand from the environment in string values
  (never keys); a missing variable without a default stops startup and names
  it; `$$` is a literal `$`. Quote the value inside YAML flow maps (`{ a: "${X}" }`).
- `agents.main` is required: the default agent, which answers a grant that
  names no `agent`. More agents sit next to it under `agents.<name>`.
- `agents.<name>.settings.context_window` is required (there is no auto-discovery):
  the model's real window. History is compacted at 80% of it, leaving room
  for the reply and the step's tool results.
  Set it somewhat below the model's real limit.
- `agents.<name>.mcp` omitted = all MCP servers, reached through `hfy__tool_search`
  (small prompt). Set it to restrict, and the tools are listed directly.
- Providers have a `type`: `openai` (the default) is the current
  OpenAI-compatible HTTP client; `mock` is an in-process adapter for dev
  and testing. It never calls the network. It streams a deterministic
  reply (or an echo), with optional reasoning, one optional configured
  tool call, `error_status` and delay. It runs the full stack, the embedded UI included, with no
  model server:

  ```yaml
  providers:
    mock:
      type: mock
      mock: { reply: "Hello from mock" }
  ```

### Model request controls

`settings` on `agents.<name>`, `subagents.<name>` and `summarizer` takes
four optional controls. Each is sent only when set, and none has a default:

- `reasoning_effort`: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`,
  sent as `reasoning_effort`. Providers and models accept different subsets; see
  [Provider notes](#provider-notes).
- `max_output_tokens`: caps each call's output, reasoning included. It is sent as
  `max_tokens`, or as `max_completion_tokens` when the provider sets
  `max_tokens_param: max_completion_tokens`.
- `call_timeout`: the longest a whole chat call may take, retries included. Past it
  the turn fails with "model call exceeded call_timeout" and the call is not retried,
  since the budget is spent. If it fires mid-stream, the text already streamed to
  clients stays and the turn fails, as it does on the provider's idle timeout.
  The provider's `timeout` is a
  different limit: the longest silence, which a long answer that keeps streaming never
  hits.
- `extra`: a map merged into the request body after hfy's own fields, for the
  vendor's extra fields. It cannot set `model`, `messages`, `tools`, `stream`,
  `stream_options` or `n` (in any letter case), nor a field the settings above already
  send; startup names the key that clashes, and two keys that differ only in case are
  refused. Values must be JSON. `extra` is stored with the request in
  `outbound_requests` (kept for `retention.trace`), so never put a secret in it. The
  trace keeps the body only, never headers; inline base64 images and signed-URL
  credentials are redacted. A trace row's error text (a provider's or an MCP
  server's) is cut to 2 KiB with every URL replaced by `<url>` and the provider's
  own API key removed; a request or response that Postgres would refuse is stored
  as `{"dropped":"not storable"}`.
  - `tool_choice`, `parallel_tool_calls`, `response_format`, `store` and `user` are
    not reserved, so they pass through unchecked. `tool_choice: required` (or a forced
    named tool) makes every reply call a tool, so the turn never ends on a text answer:
    it runs until a `limits` field or a fixed loop bound stops it. Anthropic's
    OpenAI-compatible endpoint returns 400 on a forced `tool_choice` for the Claude 5.5
    models, and Ollama ignores `tool_choice`.

```yaml
agents:
  main:
    model: openrouter/anthropic/claude-sonnet-5.5
    settings:
      context_window: 200000
      max_output_tokens: 8192
      call_timeout: 120s
      extra: { cache_control: { type: ephemeral }, reasoning: { effort: low } }
```

OpenRouter reads `reasoning_effort` and also its own `reasoning` object (options such
as `exclude`, or a token budget). Put that object and top-level `cache_control` in
`extra`; set one or the other, not both. The example above sets no `reasoning_effort`.

#### Provider notes

- **Anthropic** (its OpenAI-compatible endpoint) silently ignores `reasoning_effort`.
  Enable thinking with `extra: {thinking: {type: enabled, budget_tokens: N}}`, where N
  is at least 1024 and below `max_output_tokens`.
- **DeepSeek**: a `thinking` object in `extra` overrides `reasoning_effort`, so set one
  or the other.
- **Reasoning models**: the output cap covers reasoning too, so a small
  `max_output_tokens` can return an empty reply with `finish_reason: length`.
- **Output-cap field**: Ollama and DeepSeek read only `max_tokens`. OpenAI prefers
  `max_completion_tokens`: `max_tokens` is deprecated there and o-series models reject
  it, so set `max_tokens_param: max_completion_tokens` on that provider. OpenRouter
  accepts both.
- **Effort values**: a value the model does not support is either rejected (the turn
  fails with the provider's error) or silently ignored or mapped, depending on the
  provider.
- **Alias values**: the `reasoning_effort` list is strict. For a value it does not
  list, leave `reasoning_effort` unset and send `extra: {reasoning_effort: <value>}`;
  hfy does not check it.
- **Sampling**: newer reasoning models and Claude 4.7+ reject `temperature` below 1 or
  `top_p` other than 0.99. Leave both unset for them.
- **Gemini** (its OpenAI-compatible endpoint): vendor extras nest, as in
  `extra: {extra_body: {google: {...}}}`. `none` effort works only on the 2.5 models.
- **`mock`**: the mock provider ignores the controls and `call_timeout`; config
  validation still runs.

## API

The API is versioned in the path (`/v1`). Within `v1`, changes are additive
only: new endpoints, new optional fields, new event types. A breaking change
would come as `/v2`, served alongside `/v1`.

Your backend (`Authorization: Bearer <server.secret>`; the scheme name is case-insensitive here and on the client routes):

- `POST /v1/conversations` → `201 {"id": uuid}`. A conversation is a room;
  create it once and keep its id.
- `POST /v1/grants` `{conversation, actor, scope?, agent?, vars?, expires_in?, max_lifetime?}`
  → `201 {"id": uuid, "token": "hfy_…", "expires_at": time}`. Gives one client
  access to one conversation as one actor; hand it the token. Each person in a
  room gets their own grant.
  - `actor`: who this is (your user id; at most 256 bytes). It is recorded on
    their messages and is what other speakers and the model see.
  - `scope`: a label for whose data the conversation belongs to, default
    `actor`. hfy does not interpret it; it reaches your MCP servers in `_meta`.
  - `agent`: which configured agent answers them (`agents.<name>`; default
    `main`), so one deployment can run, say, a store agent for buyers and an
    admin agent for staff.
  - `vars`: fill the agent's declared `{{vars}}`. They render into the system
    prompt, so only your backend sets them, never a client. An undeclared or
    a missing required var is a `400` here.
  - `expires_in`: seconds, default 3600, at most 86400. Whole numbers only.
  - `max_lifetime`: seconds; makes the grant renewable (see "Renewing a grant").
    Absent, the grant is not renewable, as before. At most 604800 (7 days) and
    greater than `expires_in` (equal would end at its cap when minted); a renewable
    grant needs `expires_in` of at least 60.
    A float, exponent form, string, negative or zero is `400`.

  The token is returned once; hfy keeps only its SHA-256. A lost token cannot
  be recovered: create another grant.
- `DELETE /v1/grants/{id}` → `204`. Ends the grant now (revoking twice is fine;
  an unknown id is `404`). Open streams end within 15 s. Pass the `id` the
  mint returned: for a renewable grant that ends every token it was exchanged
  for as well, a token still in its grace included, and no exchange can outrun it.

Renewing a grant. A grant minted with `max_lifetime` lets the client swap its token
for a fresh one without your backend: the same actor, scope, agent and vars,
a new lifetime of `expires_in`, never past `max_lifetime` after the mint.

- `POST /v1/conversation/grant/exchange` (`Authorization: Bearer <current token>`;
  never `?token=`) → `200 {"id": uuid, "token": "hfy_…", "expires_at": time, "renewable": bool}`.
  `id` is the one the mint returned, so you revoke by the id you stored. The new
  token is shown only here. `renewable` is `false` once `expires_at` reached
  the cap: that token is the last, and the next one has to come from your backend.
  - `409`: the grant is live but cannot renew: it was not minted with
    `max_lifetime`, or its token already ends at that cap (the same answer, so a
    client never loops).
  - `429` `{"error": "too early to renew", "retry_after": N}` with a
    `Retry-After: N` header, N at least 1 (the body too, since a page on another origin
    cannot read the header): a token is exchanged only once a quarter of its
    `expires_in` has passed, so a chain grows by one row per quarter lifetime at
    most, whoever asks. The embedded client asks at half or later, and on a `429`
    keeps its token and waits `retry_after` seconds.
  - `401`: any other token, a retired one included, with the same body as for every
    dead token. A `403` follows the usual rule (the grant's agent is gone).
  - One of several concurrent exchanges of the same token wins; the others get `401`.
- The old token keeps working for 30 seconds after its exchange (requests in
  flight, a second tab), then is dead. It cannot be exchanged again. The
  embedded client reconnects its stream with the new token, resuming from the
  last event; the old stream ends within the grace plus up to 15 s (the stream
  re-checks its grant every 15 s).
- hfy does not detect a replayed old token. While a token is stolen and the
  legitimate client keeps renewing, both hold a live chain until `max_lifetime`
  or your revoke: pick `max_lifetime` as short as the product allows.

The client (`Authorization: Bearer <token>`; on `/events` also `?token=`, for
`EventSource`). The grant names the conversation, so these paths name none. A
token that is missing, unknown, expired or revoked is `401`; a grant whose
agent the config no longer has is `403`. Pages on another origin need it in
`server.cors_origins` (exact origins as the browser sends them, or `["*"]`).
Only these routes answer browsers, errors included; the backend routes never
do, so the server secret cannot work from a page. Tokens are never cookies, so
there is no credentials mode.

- `POST /v1/conversation/messages` `{text, attachments?}` → `202 {"turn": uuid}`.
  Starts the turn, or queues it behind the active one.
- `POST /v1/conversation/uploads` `{mime, size}` → `{"file": key,
  "upload_url": url, "expires_at": time, "direct": bool}`. Creates an
  upload slot (needs `files.dsn`); `size` in bytes is required, positive
  and bounded by `files.max_bytes`. A slot lives at
  `uploads/<conversation>/…` and is temporary: retention deletes slots
  after `files.upload_ttl` + 1h. PUT the file bytes to `upload_url`
  as the slot response directs (`direct` = a presigned S3 URL; otherwise
  an hfy path, with the grant), then post the message with `{file, mime}`
  attachments naming the slot. Posting verifies the stored slot (size,
  type, conversation) and copies the file to
  `conversations/<conversation>/…`; events name only that verified key.
  An attachment may carry a `name` (the original filename; display text
  only, stored cleaned, at most 255 bytes, no newlines). Every stored file
  is also indexed in `conversation_files` — conversation, message event,
  uploader, key, type, size — written in the message's own transaction.
- `GET /v1/conversation/config` → `{"uploads": null | {"types": [mime…],
  "max_bytes": N}}`. What the embed page config injects there: whether to
  offer attachments at all, and which types and size to check before calling
  `POST /v1/conversation/uploads` (the server checks again). `null` when no
  `files.dsn` is configured.
- `GET /v1/files/{key}` serves a stored file. It is the target of signed
  URLs hfy hands out, not a client API to construct.
- `GET /v1/conversation/events` SSE. `id` = event uuid, `event` = type, `data` = envelope.
  Resume with `Last-Event-ID`, or `?after=<event uuid>` on a first connection.
  Without either it starts from now. A `: ping` comment arrives every 15 s.
  The stream ends when the grant does (a renewable grant's old token: within its
  grace plus up to 15 s); reconnect with a fresh token.
- `GET /v1/conversation/history?limit=&before=` durable events grouped by turn,
  newest page first, keyset-paginated by turn uuid. Returns `next` (cursor for
  older turns), `live_after` (open `/events?after=<live_after>` to continue
  without a gap) and `viewer` (the grant's own actor, in the same shape as
  event actors, so a client can tell its own messages apart).
- `POST /v1/conversation/approvals` `{call_id, approve, result?, answers?}`. Only the
  actor who started the turn decides its calls (`403` otherwise). For a paused
  `hfy.ask_user` call, approving takes `answers` (a `{question id: value}` object,
  ≤ 32 KiB, one answer per question, `required` ones answered) and it is validated
  against the stored questions (`400` names the question); declining is plain
  `{call_id, approve: false}` and the model is told the user declined.
  `answers` are accepted the same way for a tool call that asked its own
  questions (below); on a plain approval they are a `400`.
- `POST /v1/conversation/stop` stops the caller's own turns: the active one
  if they started it, and their queued ones. Everyone else's keep their place.

Bodies are JSON (whatever the `Content-Type` says). Errors are
`{"error": "..."}`: `400` bad input (an empty message, a bad attachment),
`401` no valid secret or token, `403` not your turn, `404` unknown
conversation or grant, `409` no pending approval, or an exchange of a grant that cannot renew, `413` a body over 2 MiB,
`429` an exchange before its time
(an upload PUT is instead bounded by `files.max_bytes`).

### Embedded UI and JS client

hfy serves a ready-made conversation UI and the JS client it is built on,
both embedded in the binary (no npm, no build step, no extra hosting):

- `GET /embed` — the conversation page. Embed it in an `<iframe>` or a
  WebView; those are the only two supported shells. There is no script-tag
  mode and no URL-parameter configuration: `/embed` is a bare URL.
- `GET /embed/client.js` — the foundational client: a DOM-free protocol
  unit. Commands go down as promise methods (`connect`, `send`, `upload`,
  `approve`, `stop`, `loadOlder`); notifications come up as fired events
  (the raw event envelopes, plus connection state). It owns the grant in
  memory, token refresh, resume and dedupe, and knows nothing about
  rendering, locale or theme. `ui.js` (the UI) is built on it; custom
  UIs may use it directly.
- `GET /embed/ui.js`, `GET /embed/ui.css`, `GET /embed/alpine.min.js` — the
  UI itself (Alpine.js, vendored into the binary, provides reactivity).

Caching and compression (nothing to configure). Every file of the embedded UI
is served under two URLs:

- `/embed/<name>` (`/embed/ui.js`, `/embed/client.js`, ...) keeps working
  forever, so hosts and WebViews that pinned it do not break. It is sent with
  `Cache-Control: no-cache` and an `ETag`: the client revalidates and gets
  `304` while nothing changed.
- `/embed/assets/<name>.<hash><ext>` (`/embed/assets/ui.3fa9c1d2e4b5.js`) is
  named by the first 12 hex digits of the file's sha256, so its bytes never
  change: `Cache-Control: public, max-age=31536000, immutable`. `GET /embed`
  (the page, always at that one URL, `no-cache` + `ETag`) points at these URLs,
  so an upgrade reaches clients with the next page load and nothing is
  re-downloaded otherwise. A hash that is not the current one (an old page
  open across an upgrade, or a rolling deploy behind a load balancer, where a
  page from one version reaches an instance of another) is not an error:
  when the name before the hash and the extension is a known file
  (`ui.0123456789ab.js` asks for `ui.js`), the instance answers `200` with
  its current bytes, `Cache-Control: no-store` (never `immutable`, so no cache
  keeps mismatched bytes under a hash) and the same `ETag`, `Vary` and
  encoding as the current URL. A mixed fleet therefore never leaves a widget
  without its scripts, and a page that ended up with files of two versions is
  fixed by the next page load. An unknown name (or a malformed hash: 12
  lowercase hex digits) is a plain `404`. The page's CSP and the bridge are
  unchanged, and a bare `/embed` URL still works in a WebView. All of
  `/embed/*` shares one metrics route label, `GET /embed/{name...}`.

Text files, and the page, are compressed once when the server starts (brotli
and gzip, about 0.3 s for the whole UI) and the bytes are reused.
`Accept-Encoding` picks `br`, `gzip` or the plain file by q-value (`br;q=0`
and `identity;q=0` are honoured; an unparseable or oversized header means
plain; a client that refuses every encoding we have gets `406`), and `Vary: Accept-Encoding` is always sent. Each
encoding has its own strong `ETag`, the hash of the bytes sent; `If-None-Match`
takes lists, weak tags (`W/"..."`) and `*`. `HEAD` answers like `GET` without a
body. `Range` is not supported: the header is ignored and the whole file is
sent with `200`. A reverse proxy in front may compress too: we set
`Content-Encoding`, so a proxy that honours it will not compress twice. The
first load of the UI (the page and the files it names) is held under 60 KiB
of brotli by a test.

The grant flow is the integration. Your backend (server secret) creates
the conversation once and mints a short-lived grant per viewer; your page
or app hands `{token, expires_at}` to the widget, which keeps it in JS
memory only — never cookies, web storage, or a URL (the one exception is
`?token=` on the SSE stream, where `EventSource` cannot set a header).
On expiry or `401` the widget asks for a fresh grant and resumes the
stream from its last event; your backend re-mints only after its own
login checks. A grant extends itself only when you minted it with
`max_lifetime` (see "Renewing a grant"): the widget then exchanges its token
before expiry, up to that cap, and asks you only when the exchange fails or the
cap is reached. Grants never travel in a URL query (a fragment bootstrap must
be erased immediately after reading).

Bridge protocol (envelope `{"v":1,"type":…}`): the widget sends `ready`
on load and `token-needed` when it needs a grant; the host answers
`grant` (`{token, expires_at}`) and may send `configure` (below). Always
answer `token-needed`, even for renewable grants: it comes with reason
`initial`, and again when an exchange fails or the cap is reached (reason
`expired`) or a request is refused (`unauthorized`). When you re-mint after
`unauthorized`, revoke the previous grant's `id`; that ends its whole chain.
Outbound, the widget calls `window.HfyBridge.postMessage(json)` when
present (Android `addJavascriptInterface`); React Native and other
shells inject a one-line shim onto their native bridge — and otherwise
sniffs the known bridges (`ReactNativeWebView`,
`webkit.messageHandlers.hfy`, `chrome.webview`), falling back to
`window.parent.postMessage` only when genuinely framed. Inbound, every
shell calls `window.hfy.receive(json)`. `window.parent` is never
redefined: it is a built-in that equals `window` in a top-level
WebView. Flutter apps: use the native hfy_flutter SDK (in development);
do not embed `/embed` in a WebView.
For iframes, parent origins are allow-listed by `server.cors_origins`,
checked on every `postMessage`, and emitted as CSP `frame-ancestors`.

Presentation is configured in the main YAML (`embed:`: `lang`, `dir`,
`theme`, `accent`, `density`, `layout`, `tool_calls`), validated at startup and injected
into the page. `tool_calls` is how much of the agent's tool use viewers see:
`hidden`, `summary` (default: a friendly one-line row, never JSON) or
`details` (arguments and results, for developers); approval requests always
show, since the viewer must decide them. `layout` picks the look: `bubbles`
(default: tails, wallpaper, colours derived from `accent`), `transcript` (plain
rows, no bubbles or wallpaper) or `whatsapp` (bubbles with WhatsApp-style colours
that follow `theme`: green outgoing, white incoming, wallpaper, a composer bar;
bubbles up to 65% of the width and 80% on phones). In `whatsapp` the accent is
WhatsApp green unless `accent` is set in YAML or by `configure`; a set accent
drives the send button, links, approvals, focus and the new-messages pill, and
tints the viewer's own bubbles (timestamps keep 4.5:1 on any tint, and text on an
accent fill is picked for 4.5:1). `density` (`comfortable`, `compact` or `spacious`),
`lang`, `dir` and `tool_calls` apply as in the other layouts. Per-viewer overrides
arrive only via the `configure` bridge message from your glue (which can read
your page's own `lang`/`dir`, or your app's selected locale; `layout` can be
set there too; an `accent` there must be a six-digit hex such as `#e11d48`, and any
other value is ignored). Timezone, language and
direction otherwise auto-detect from the viewer (`Intl`), with built-in
English and Arabic strings. Arbitrary CSS/HTML and trust fields (token,
API base, actor, agent, vars) are never configurable from the client.

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
| `hfy_provider_requests_total`, `hfy_provider_request_duration_seconds` | `provider`, `kind` (`chat`), `outcome` |
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
don't know. Served `message.text` attachments for stored files carry an
ephemeral `url` (a fresh signed URL); stored events keep only `{file, mime}`.

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
  the model as error results (an unknown tool lists the valid names). The
  database cannot store NUL characters, unpaired surrogate escapes or numbers it
  would expand past reason, so arguments holding any of them, or a number over
  1000 digits once written out, are answered as invalid arguments; a tool name
  holding them or over 256 bytes is an unknown tool, stored cleaned and cut; a
  call id holding them, over 256 bytes, empty or already used in the conversation gets a
  generated unique one; a tool's
  structured result, card or questions that cannot be stored are dropped with a
  warning; and model and tool text is stored with NUL characters removed. Every
  string, key and number is checked, also under a repeated key (Postgres parses
  them all and keeps the last), and nesting deeper than 64 levels is refused. The
  size counted is what Postgres reads back: a space after every `,` and `:`, and
  each number at its written-out length (it writes `1e100000` out in full), so
  that must fit 64 KiB. Valid but extreme values are refused too (a number of
  over 1000 digits, `0e1000`). A call that repeats an
  earlier one in the same turn (same tool and arguments; key order does not
  matter) is refused with advice from its third occurrence, and the sixth ends
  the turn. The count survives a resume: a turn that crashed, waited for approval
  or asked the viewer is counted from its own recorded calls. Known limitation:
  numbers written in exponent form (`1e2` vs `100`) can get a different repeat key
  after a resume, because Postgres stores `1e2` as `100`. `hfy.ask_user` counts
  too unless the viewer answered it: an answered ask never counts, so a loop of
  answered questions runs on, while a declined, timed-out or still-pending ask
  counts, and the same questions asked a third time are refused without pausing
  the viewer again. Fixed bounds, not configurable: one call's arguments over 64 KiB
  are not run, the model is told to send less, and only a marker is stored; a turn
  with 5,000 durable events ends failed; a request over 8 MiB of text is compacted
  first (main agent only, and only with a `summarizer` configured) and, if still
  too large, the turn fails before sending. `outbound_requests` stores the
  provider's raw response, including full oversize arguments, until
  `retention.trace` expires: the 64 KiB bound protects events and prompts, not
  that trace. Sub-agents and the summarizer follow
  the same bounds, and their steps count toward the turn's events; a sub-agent's
  repeats are counted within its own run. Model calls retry on 429, 5xx and
  network errors, with at most `queue.max_attempts` attempts in all, but a call
  past its `call_timeout` is not retried. A provider's `timeout` is the longest
  silence allowed (before the answer starts, then between streamed chunks), so
  long answers are never cut off. `limits` (`steps`, `total_tokens`, `timeout`)
  end the turn when hit.
- **Approvals.** Default is to never ask. Calls matching `agents.<name>.approval.tools`
  (`<server>.<tool>` patterns, `hfy.*` works) pause the turn; nothing waits in
  a worker. After approval.timeout the call is rejected. Each agent has its own
  `approval`. A server's own `mcp.<name>.approval` (tool patterns without the
  server prefix, e.g. `[refund, delete_*]`) applies to EVERY agent; an agent's
  `approval.tools` only adds more. `mcp.<name>.approve_destructive: true`
  waits on tools whose annotations say destructive (readOnlyHint not true AND
  destructiveHint explicitly true); unannotated tools never count. The core
  built-ins (hfy.now, hfy.math_calc, hfy.tool_search)
  never ask, even when a pattern like `hfy.*` matches. hfy.ask_user pauses the
  same way and never twice: it always asks the viewer, even if a pattern
  matched its name; it runs after `approval.timeout` (the agent's own if it
  has one, else a fixed 30m).
  Approval covers an agent's own calls only: a sub-agent's own tool calls are not checked, so if a sub-agent
  can reach a guarded tool, guard the sub-agent itself (`hfy.<name>`).
- **Core tools.** Every agent gets hfy.now {timezone?} (UTC, and local time,
  weekday and offset for an IANA zone) and hfy.math_calc {expression}
  (arithmetic, comparisons and math functions via
  [expr](https://github.com/expr-lang/expr), math built-ins only; numbers
  never wrap around and results drop float noise), served by the built-in `hfy`
  server (the name is reserved; no sub-agent may shadow a built-in name). A
  tool is core only if it is pure (no I/O, no app data, no permissions) and
  does what models get wrong alone; anything touching data belongs to your
  MCP servers.
- **Asking the viewer.** A top agent may opt into `hfy.ask_user`
  (`agents.<name>.ask_user: true`): one
  call pauses the turn with up to 10 structured questions (single/multi with
  2–20 options, free text, confirm) the viewer answers as a form. The model
  receives the answers as the tool's result: the JSON
  `{"answers": {"<question id>": value}}` (single: an option value, multi: an
  array of them, text: a string, confirm: true/false), or
  "The user declined to answer." when the viewer skips, "The user did not
  answer in time." after the timeout above. Without the opt-in the tool does
  not exist for that agent (an API-only app would stall); invalid arguments
  come back as an error result without pausing; sub-agents never ask.
- **Cards and questions from tools.** A tool that opts in may ship its own
  display card in `_meta["hfy/ui"]`, and, when its server allows, structured
  questions that attach to the tool's real call. Before anything reaches a
  viewer it is rebuilt server-side from the allowlist below; anything the
  schema does not know is dropped, strings are clipped and cleaned, and the
  model never sees the card.

  Enable per MCP server (`mcp.<name>.ui`) — but only after every hfy
  instance is upgraded. Older binaries never read `_meta`, so cards are
  inert there; worse, an older worker resuming a held ask after the
  viewer's answer was recorded ignores the unknown fields on the stored
  `approval.requested` and re-executes the tool. Upgrade all instances
  and drain in-flight held asks before enabling `ui:` anywhere; strict
  config (unknown keys stop startup) only protects a fresh start.

  ```yaml
  mcp:
    shop:
      type: local
      command: ["node", "shop-mcp.js"]
      ui:
        image_hosts: ["cdn.shop.example", "*.shop-cdn.net"] # https only; exact host or *.suffix
  ```

  Allowed shapes (unknown fields are dropped, not fatal):

  - `{v: 1, type: "card", title?, subtitle?, body?, badge?, kv?: [{label,
    value}], image?, buttons?}` — title or body required. At most 6 kv rows,
    3 buttons; title/subtitle ≤ 120 chars, body ≤ 500, value ≤ 120, image ≤
    2048 bytes and only https on a configured `image_hosts` host; a broken
    image drops only the image.
  - `{v: 1, type: "carousel", cards: [...]}` — the first 10 usable cards.
  - `questions: {"questions": [...]}` — same shape as `hfy.ask_user`'s
    arguments. Questions reach the viewer only when the agent itself may ask
    (`ask_user: true`, top agents): otherwise they are dropped and the card
    survives. A block that is only questions (no card, or a card that fails)
    keeps the questions, so the tool just asks with nothing to render. An
    over-total (32 KiB) or v-not-1 block is dropped whole.
  - buttons: `{send: {label}}` (label is sent verbatim as a new viewer
    message), `{link: {label, url}}` (https, opens in a new tab), or
    `{host: {label, name, payload?}}` (name one lowercase letter then
    `a-z0-9_.-`, at most 64 characters, payload a JSON
    object ≤ 2 KiB; the message goes to the host app, see below).

  When a tool's call carries questions, the turn pauses on that same call
  (no second, synthetic ask): the embedded UI answers them in the approval
  sheet with the card beside it, and the model then receives the tool's own
  result joined with `{"answers": {...}}` — exactly one `tool_result`.
  Declining, timing out (`approval.timeout`, else 30m) or stopping the turn
  all end with the held result plus a plain note; a stop keeps the card but
  adds "The user stopped before answering.". Approval-gated tools cannot
  ask: their calls run only after the approval, and any questions they
  return then are dropped with a log line.
  Host actions reach the app embedding the widget as
  `{"v": 1, "type": "action", "name": "<host.name>", "payload": <object>,
  "call_id": "<the tool call>"}` over the same bridge as tokens
  (`window.HfyBridge`, webkit messageHandlers, chrome.webview,
  ReactNativeWebView, or postMessage from the framed widget).
  Send/host buttons are clickable only while the conversation ends in their
  own turn and the viewer started it; link buttons are always live. Cards
  render in every `tool_calls` mode.
- **Sub-agents** are tools (`hfy__<name>`), isolated (task text only), depth 1;
  their usage counts toward the parent's turn. Every agent may call them.
- **Tool servers know who is calling.** Every MCP tool call carries `_meta`
  `"hfy/turn": {conversation, turn, call_id, agent, actor: {type,
  ref}, scope, vars}`, so your MCP server can authorize each call itself (the
  actor is whoever started the turn; `vars` are the grant's, so keep
  secrets out of them: every MCP server, remote ones too, receives them). Return
  `structuredContent` for data meant for the client; the model sees the text.
   Put a display card in `_meta["hfy/ui"]` (the next section) for the viewer.
- **A conversation is a room.** Every agent that answers in it sees the whole
  conversation: its own messages and tool calls as its own, and, once others
  speak (another agent, or a second person), everyone else's messages framed
  with who wrote them: `<message from="u1" kind="person">…</message>`. The
  text inside is escaped, so nobody can forge a frame, and the system prompt
  explains them. Other agents' tool calls stay theirs. A one-to-one chat is
  sent plain. Frames exist only in the prompt (stored events and the API are
  unchanged); make actor refs readable (the grant's `actor`) if the model
  should know names. Frames tell the model who spoke; they are not
  authorization: tools decide from `_meta`, which the model cannot change.
- **Memory.** The summarizer appends a `summary` event when the next prompt
  would pass 80% of `context_window` (history is never deleted). It folds
  earlier turns, never the running one, so a single turn with very large tool
  results can still outgrow the window. If summarizing fails, hfy logs it and
  the turn goes on uncompacted, without a second try in that turn. hfy has
  no recall of its own: to let an agent search older conversations, expose
  that as an MCP tool (filtered by `_meta.actor`).
- **MCP servers.** A local server inherits only `PATH`, `HOME`, `USER`, locale,
  `TMPDIR` and `TZ` from hfy, never its secrets; give it anything else in
  `environment`. Each tool call has `timeout` (default 2m) to answer.
- **Files.** The optional `files:` block lets files reach a conversation, from
  two sources that share one `types` list (lowercase MIME types, no wildcards;
  required) and one `max_count` (per message, both kinds together, default 10):
  - *URL attachments* are references your app hosts. They are on when
    `allowed_hosts` is set (exact names or `*.suffix`; lowercase, no scheme,
    port or path; IDN as punycode). Host and type are checked when the message is
    posted; hfy never fetches the URL, so `max_bytes` does not apply to them.
  - *Uploads* are files hfy stores. They are on when `dsn` is set
    (`s3://…` with `region=` required in the query, or `file://…`;
    `file://` needs `public_url` so providers can reach hfy-served files).
    `max_bytes` (default 10MB) bounds each file; `upload_ttl` default 5m (at
    most 1h), `download_ttl` default 15m (at most 24h). `files:` needs at least
    one of `allowed_hosts` and `dsn`, and an empty `dsn` is an error, not "off":
    remove the key instead.

  Images go to the model only in the turn
  they arrive in, always as their URL: fetching it is the provider's job, so
  the URL must be reachable from the provider (a presigned URL works well).
  A local model server (Ollama, llama.cpp) only sees images it can fetch
  itself: with `file://` storage, set `public_url` to an address it can
  reach from its host. Non-image files reach the model only as their key,
  never as data. Uploads work like this:
  `POST /v1/conversation/uploads` takes `{mime, size}` and
  returns a slot under `uploads/<conversation>/…` (retention deletes slots
  after `upload_ttl` + 1h; an S3 lifecycle rule on `uploads/` is a useful
  backstop), the message carries `{file, mime, name?}` naming the slot,
  and hfy verifies the stored slot and copies the file to
  `conversations/<conversation>/…` — events name only that verified copy,
  and every stored file is indexed in `conversation_files` (conversation,
  message event, uploader, key, type, size) in the same transaction.
  With S3, the bucket needs CORS for PUT from the embed origins.
  Downloads are served via transient signed URLs (put/get signatures are
  domain-separated): stored events keep only `{file, mime}`; served
  payloads carry a fresh `url`.
- **Retention.** Ephemeral events, `outbound_requests`, finished jobs and
  grants that ended expire after `retention.trace`; durable events and usage
  are permanent.
- Tool names reach the provider as `<server>__<tool>` (`files.read` →
  `files__read`); events and approval patterns use `files.read`.

## Upgrading to v2

- `attachments:` and `filesystem:` are now one flat `files:` block. A config that
  still sets either key stops EVERY command that reads the config (`serve`,
  `queue:work`, `validate`, `migrate`) with
  `attachments was removed in v2; merge it into files: …` or
  `filesystem was removed in v2; merge it into files: …` (an old key is reported
  even when `files:` also has a mistake). Move the fields as below; nothing else
  changes (the `{file}` / `{url}` attachment payloads, the upload protocol and
  `GET /v1/conversation/config` are the same).

  ```yaml nocheck
  # before (v1: refused by v2)
  attachments:
    allowed_hosts: [cdn.example.com]
    types: [image/png, image/jpeg]
    max_bytes: 5MB
    max_count: 10
  filesystem:
    dsn: s3://hfy-uploads?region=eu-central-1
    types: [image/png, image/jpeg, application/pdf]
    max_bytes: 10MB
    upload_ttl: 5m
    download_ttl: 15m
  ```

  ```yaml
  # after
  files:
    types: [image/png, image/jpeg, application/pdf]  # the union of both lists
    max_count: 10
    max_bytes: 10MB
    allowed_hosts: [cdn.example.com]
    dsn: s3://hfy-uploads?region=eu-central-1
    upload_ttl: 5m
    download_ttl: 15m
  ```

  | v1 | v2 |
  |---|---|
  | `attachments.allowed_hosts` | `files.allowed_hosts` (setting it turns URL attachments on) |
  | `attachments.types` and `filesystem.types` | `files.types`, ONE list for URLs and uploads: use the union |
  | `attachments.max_count` | `files.max_count` (default 10; it also caps uploads when no URL attachments are set: that was a fixed 10 before) |
  | `attachments.max_bytes` | dropped: it never limited anything (hfy does not fetch URLs) |
  | `filesystem.dsn` | `files.dsn` (setting it turns uploads on) |
  | `filesystem.public_url`, `max_bytes`, `upload_ttl`, `download_ttl` | the same names under `files` |

- If your two lists differed, `files.types` is their union: a type that was
  upload-only (a PDF) is now also accepted as a URL attachment, and the other
  way round. Non-image files reach the model only as their key, never as data.
- `files.max_bytes` defaults to 10MB, as documented. In v1 it was 5MB when
  `attachments:` was also set (it inherited `attachments.max_bytes`' default).
  Set it explicitly to keep a smaller limit.
- Stricter checks, reported at startup: `types` entries must be lowercase
  `type/subtype` (no `*`, parameters or duplicates), `allowed_hosts` entries
  lowercase hosts (`localhost`, IPs and `*.example.com` are fine; no scheme,
  port, path, trailing dot or duplicates; `*.com` is refused), and an empty
  `dsn` (an unset `${VAR}`) is an error instead of quietly switching uploads off.
  Sizes that overflow (`9999999999GB`) are now invalid.
- The top-level `agent:` config key is gone; `agents:` is the only home for
  agents. Move the contents of `agent:` under `agents.main:` (same fields,
  indented one level). A config that still sets `agent:` stops EVERY command that
  reads the config (`serve`, `queue:work`, `validate`, `migrate`) with
  `agent was removed in v2; move it to agents.main`. `agents.main` is required:
  grants that omit `agent` still run it, and existing grants and turns need no
  migration. Error paths now read `agents.main.settings…`, not `agent.settings…`.
- The `embeddings:` config key and the `hfy.conversation_search` tool are gone.
  A config that still sets `embeddings:` stops EVERY command that reads the
  config (`serve`, `queue:work`, `validate`, `migrate`) with
  `embeddings was removed in v2; expose memory as an MCP tool`: delete the key,
  and the mock provider's `embed_dimensions` with it. To keep recall, expose it
  from your own MCP server as a tool.
- Migration `00003` drops the old `embeddings_<dims>` tables, any unfinished
  `embed_events` jobs and the `message.text` index that only the embeddings
  backfill read, when hfy starts. The `vector` extension is left in place
  (`DROP EXTENSION vector` is yours to run if nothing else uses it). hfy now
  runs on plain `postgres:17`; pgvector is no longer needed.
- Do not run v1 and v2 against the same database side by side. A v1 instance
  keeps enqueuing `embed_events` jobs that no v2 worker claims: they stay queued
  forever and `hfy_jobs{queue="embeddings",state="queued"}` never drops. A v1
  instance that restarts after `00003` ran recreates `embeddings_<dims>`, and the
  migration will not run again. Remove `embeddings:` from the v1 config, or stop
  every v1 instance, BEFORE starting v2.
- The only queues now are `turns` and `maintenance` (the default for
  `queue:work`). Drop `embeddings` from `queue:work -q`: naming it is not an
  error, the worker just polls that queue and finds nothing it can run, since
  no `embed_events` handler exists.

## Upgrading to renewable grants

- Migration `00004` adds three nullable columns to `grants` (no table rewrite, no
  lock beyond the catalog) and an index on `root_id`. It gives up after 5 s if a
  long transaction holds `grants` (then run it again), rather than block token
  lookups behind its `ALTER`. Grants that exist, or that
  an older instance mints during a rolling deploy, are chains of one and are never
  renewable.
- Do not mint grants with `max_lifetime` until every instance runs this version.
  An older instance ignores the field (the grant is simply not renewable), and its
  revoke ends only the row it is given, so the later tokens of a renewable chain
  would outlive it.
- `hfy migrate down` past `00004` ends the later tokens of every renewable chain
  first (the older version could not revoke them), so those clients ask your
  backend for a new grant.

## Development

```sh
docker run -d -p 127.0.0.1:54330:5432 -e POSTGRES_PASSWORD=hfy -e POSTGRES_DB=hfy postgres:17
golangci-lint run ./... && go test -race ./...   # exactly what CI runs
```

`make check` runs the whole CI gate (`go mod tidy -diff`, lint, then the tests
with `HFY_TEST_REQUIRE_DB=1`); `make db` starts the test database.

Tests need a Postgres (`HFY_TEST_DB` overrides
`postgres://postgres:hfy@127.0.0.1:54330/hfy`); each test gets its own
database. Locally they skip without one; CI sets `HFY_TEST_REQUIRE_DB=1` so a
missing database fails instead. A scriptable fake OpenAI server
(`internal/provider/fakeprov`) and a fake stdio MCP server
(`internal/mcp/fakemcp`) stand in for real ones in tests. For live dev
or a demo without a model server, run against a `mock` provider
instead (see Config).

**Toolchain.** `go.mod` pins Go 1.27.3 and the `go` command switches to it,
but tools built with an older Go reject newer syntax (generic methods) with
"method must have no type parameters". Build them with the module's toolchain,
and format with `go fmt` or `golangci-lint fmt`, not a `gofmt` from `PATH`:

```sh
GOTOOLCHAIN=go1.27.3 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOTOOLCHAIN=go1.27.3 go install golang.org/x/tools/gopls@latest
```

Layout: `internal/{config,types,db,jobs,agent,mcp,files,httpapi,svc,provider,metrics}`;
`main.go` (the CLI) and `harness.go` (the process wiring) at the root;
migrations, SQL queries, prompts and the example config are embedded in the
binary.

### Conventions

Each rule says what enforces it. A rule that only lives in prose does not
survive many contributors, so prefer adding a check to adding a sentence.

| Rule | Enforced by |
|---|---|
| **Layering.** Imports follow the table in `arch_test.go`; `httpapi` never imports `agent` (API and workers scale apart), `types` imports nothing. | `TestLayering` |
| **One transaction, one connection.** A function holding a `pgx.Tx` never touches `.Pool`, nor calls a `Store` method that does: a second connection deadlocks once the pool runs out. | `TestTransactionsUseOnlyTheirConnection`, `TestMutationsNeedOneConnection` |
| **`svc` owns turn state.** Every `turn.state` change is a `svc` method under the conversation lock; the agent appends only what it produces. | `TestOnlySvcWritesTurnState` |
| **The event log is the truth.** State is folded from events (`svc.ViewOf`, `agent.buildContext`); events are never updated; payloads are read with `e.Payload[T]()`, and one that does not decode is an error. | review; `Payload` is the only decoder |
| **Stored data is versioned.** Payloads carry `v` and are upgraded on read (`types.register`); SQL-read fields are frozen. A breaking job payload change is a new job type. Migrations stay compatible with the previous release (add first, remove a release later). | `TestEventVersions`, `TestClaimOnlyHandledTypes`, review |
| **Writes that belong together commit together** (an event and the job it implies). | review |
| **Jobs are fenced by attempt** and resume from the log; side effects are never repeated blindly. | `TestStaleWorkerCannotTouchAReclaimedJob`, crash-recovery tests |
| **No silent failure.** Errors are returned, or logged with conversation and turn when nothing can act on them. | `errcheck`; review |
| **Current Go.** `slices`/`maps`, range-over-int, `new(expr)`, `t.Context()`, generic methods. | `modernize` |
| **Every `//nolint` names its linter and says why.** | `nolintlint` |

### Design notes

- No generic event router: the few places that fold events use a plain `switch` on
  the type, which is less code and easier to follow.
- The `LISTEN` connection is opened separately instead of being held out of the
  pool (same effect, simpler lifecycle).
- Sub-agent steps
  are not stored as events (their `model.usage` and `outbound_requests` are);
  attachments on later turns are a placeholder; no secret rotation.
- The provider package has one narrow `Adapter` interface (`Chat`)
  with two implementations (`openai`, `mock`). This is the
  deliberate exception to preferring concrete types: config selects the
  implementation per provider name, so the agent runtime needs the seam.
- Filesystem storage uses `gocloud.dev/blob` directly, with no homegrown
  Files interface; drivers are `s3` and `file` (`mem` in tests).

### Not in v1

Images returned by MCP tools (they become a text placeholder),
semantic `tool_search` (keyword today), structured output, provider failover,
other stream formats, an OpenAI-compatible API (serving
`/v1/chat/completions`) in front of hfy.
