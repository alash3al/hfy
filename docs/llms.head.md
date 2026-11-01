# hfy

> hfy is a self-hosted backend and embeddable chat UI for AI conversations: one Go binary plus Postgres, configured by one YAML file. The host app keeps its users and data. It creates conversations and short-lived access grants over HTTP. hfy runs the agent loop, calls tools from MCP servers, stores every step as an append-only event, streams the reply over SSE and serves a ready-made chat UI at `/embed` for iframes and mobile WebViews.

This file is the complete integration guide for AI coding agents. It is self-contained, so you should not need any other page to integrate hfy into an app. Source: https://github.com/alash3al/hfy. The part after "Troubleshooting" is the human reference (https://github.com/alash3al/hfy/blob/main/docs/reference.md) copied verbatim, so the two cannot disagree.

## How to use this guide

To add an AI chat to an existing app, follow "Integration recipe" in order. Then read the reference sections after "Troubleshooting" for exact shapes (Config, API, Events, How it behaves). Check "Rules" before you finish, and use "Troubleshooting" when something fails.

Choose the client surface first:

1. **Embedded UI (default, recommended).** iframe on the web, WebView in mobile apps. No UI code; the host only supplies grants.
2. **JS client (`/embed/client.js`).** For a fully custom web UI. A DOM-free class that handles history, streaming, reconnects, token refresh, uploads and approvals.
3. **Raw HTTP + SSE.** Native mobile UIs, or other languages.

## Concepts

- **Instance.** One hfy deployment serves one app (one tenant). Many end users share it; isolation is per conversation. Run another instance for another app.
- **Conversation.** A room, created by the host backend. Holds the full, never-deleted event history. Several people and several agents may share one.
- **Grant.** Access for one person (an `actor`) to one conversation. The backend creates it with the server secret and gets a token `hfy_…` (default lifetime 1 h, max 24 h). The client uses only that token. Tokens are never cookies.
- **Actor.** The host's own user id for the person (max 256 bytes). It is recorded on their messages and shown to the model and to other participants.
- **Agent.** A configured assistant: model, system prompt, tools, limits. `agents.main` is required and is the default agent; `agents.<name>` adds named ones. The grant picks which agent answers that person (none named = `main`).
- **Turn.** One user message and everything the agent does in response. Turns run one at a time per conversation; later messages queue.
- **Event.** Every step (message, tool call, tool result, state change, approval) is an append-only event. History and the live stream use the same envelope.
- **Tools.** All external tools come from MCP servers (local stdio commands or remote Streamable HTTP). hfy also ships core built-ins: `hfy.now`, `hfy.math_calc`, `hfy.tool_search`, opt-in `hfy.ask_user`, and sub-agents as `hfy.<name>`.
- **Server secret.** `server.secret` (usually `${HFY_SERVER_SECRET}`). Backend-only. Backend routes never answer browsers (no CORS), so it cannot work from a page.

## Integration recipe

### 1. Run hfy

Requirements: Postgres 17, and an OpenAI-compatible model endpoint (OpenAI, OpenRouter, Ollama, vLLM, llama.cpp, …). For a first run without any model, use the built-in `mock` provider (see Config).

Prebuilt binary (Linux, macOS, Windows; amd64 and arm64), from the latest GitHub release:

```sh
# pick one: linux_amd64, linux_arm64, darwin_amd64, darwin_arm64 (.tar.gz); windows_amd64, windows_arm64 (.zip)
curl -sSL https://github.com/alash3al/hfy/releases/latest/download/hfy_linux_amd64.tar.gz | tar xz
./hfy_linux_amd64/hfy version
```

Docker image: `ghcr.io/alash3al/hfy` (tags `latest`, `<major>`, `<major>.<minor>`, `<version>`; linux/amd64 and linux/arm64). Config is read from `/etc/hfy/hfy.yaml`; the default command is `serve --work` on port 8080.

Docker Compose (from a clone of the repo; starts hfy and Postgres together):

```sh
git clone https://github.com/alash3al/hfy && cd hfy
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
docker compose run --rm --no-deps hfy config:example > hfy.yaml   # commented starter config
# edit hfy.yaml (provider, model, context_window, system prompt, cors_origins)
docker compose up        # API + workers on :8080; migrations run on boot
```

Inside Docker, `localhost` is the container. To reach Ollama on the host, use `http://host.docker.internal:11434/v1` (add `extra_hosts: ["host.docker.internal:host-gateway"]` to the service on Linux).

Binary (prebuilt as above, or `go install github.com/alash3al/hfy@latest`):

```sh
hfy config:example > hfy.yaml
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
export DATABASE_URL=postgres://postgres:hfy@localhost:5432/hfy
hfy -c hfy.yaml validate          # checks config, env expansion, startup rules
hfy -c hfy.yaml serve --work      # API + workers in one process
```

Commands: `serve [--work]`, `queue:work [-q turns,maintenance] [--workers N]` (workers only, scale separately), `validate`, `migrate [up|down|status]`, `config:example`, `version`. `-c` may also come from `HFY_CONFIG`. Health: `GET /healthz` (process alive), `GET /readyz` (Postgres reachable, else 503). `metrics: {listen: "127.0.0.1:9090"}` opens a separate Prometheus `/metrics` listener (also `/healthz`, `/readyz`) in every process.

### 2. Minimal config

```yaml
server:
  secret: ${HFY_SERVER_SECRET}
  cors_origins: ["https://app.example.com"]   # every origin that iframes /embed or calls client routes from a browser

database:
  url: ${DATABASE_URL}

providers:
  openai:
    url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
    timeout: 60s

agents:
  main:                              # required: the default agent
    model: openai/gpt-4o-mini        # <provider name>/<model id>
    settings: { temperature: 0.2, context_window: 128000 }   # context_window is REQUIRED
    system: |
      You are the support assistant for {{company}}. You are talking to {{user_name}}.
    vars:
      company:   { default: "Acme" }
      user_name: { required: true }
    ask_user: true                   # optional: lets the agent ask structured questions
```

No model yet? Use the mock provider. It streams a fixed reply in-process, so the whole stack (UI included) runs without a model server:

```yaml
providers:
  mock:
    type: mock
    mock: { reply: "Hello from mock" }
agents:
  main:
    model: mock/demo
    settings: { context_window: 8192 }
```

Keys at a glance (`hfy config:example` prints every key with comments; unknown keys stop startup with their line):

- `providers.<name>`: `url`, `api_key`, `timeout` (the longest silence: before the answer starts and between streamed chunks), `type` (`openai` or `mock`), `max_tokens_param` (`max_tokens` by default; OpenAI o-series models need `max_completion_tokens`).
- `agents.<name>`: `model` (`<provider>/<model>`), `settings` (`context_window` required, `temperature`, `vision`: accept image attachments, default false, and the model request controls below), `system` (supports `{{vars}}`), `vars` (`{default: …}` or `{required: true}`), `limits` (`steps`, `total_tokens`, `timeout` per turn), `approval` (`tools`, `timeout`), `ask_user` (bool), `mcp` (server names).
- `subagents.<name>`: `description`, `model`, `settings`, `system`, `mcp`, `limits`.
- `summarizer`: `model`, `settings`, `keep_recent` (default 3), `system`.
- `files` (omit = no attachments at all): `types` (required; lowercase MIME, for URLs and uploads), `max_count` (10, per message), `max_bytes` (10MB, the largest uploaded file; hfy never fetches URLs), `allowed_hosts` (set = URL attachments allowed), `dsn` (set = uploads on: `s3://bucket?region=…` or `file:///path`), `public_url` (`file://` only), `upload_ttl` (5m, at most 1h), `download_ttl` (15m, at most 24h).
- `queue`: `workers` (4), `lease` (30s), `max_attempts` (3). `retention`: `trace` (30d). `metrics`: `listen`.

Vendor quirks of the model request controls (Anthropic and DeepSeek thinking, reasoning-model output caps, Gemini) are in "Provider notes" of the full reference: https://github.com/alash3al/hfy/blob/main/docs/reference.md.

### 3. Backend: create conversations and grants

All backend calls use `Authorization: Bearer <server.secret>` and JSON bodies: `POST /v1/conversations` returns the conversation id, `POST /v1/grants` returns the token (fields in "API" below).

Persist the conversation id in your own database (for example one per user, per support ticket or per order), and create it once. Create a fresh grant every time the widget asks for one, and only after your normal session/auth check. Never cache or reuse tokens across users. The token is returned once (hfy stores only its SHA-256). Pick `max_lifetime` (renewable grants, see "Renewing a grant") as short as the product allows, or leave it out. Do not mint grants with `max_lifetime` until every hfy instance runs this version: an older instance ignores the field, and its revoke ends only one token of a renewable chain.

Example backend endpoint (Node/Express; translate to the app's stack):

```js
const HFY = process.env.HFY_URL;                       // e.g. https://hfy.example.com
const auth = { Authorization: `Bearer ${process.env.HFY_SERVER_SECRET}`, "Content-Type": "application/json" };

app.post("/api/chat/grant", requireLogin, async (req, res) => {
  let conversation = await db.getChatConversation(req.user.id);
  if (!conversation) {
    const r = await fetch(`${HFY}/v1/conversations`, { method: "POST", headers: auth });
    if (!r.ok) return res.status(502).end();
    conversation = (await r.json()).id;
    await db.saveChatConversation(req.user.id, conversation);
  }
  const g = await fetch(`${HFY}/v1/grants`, {
    method: "POST", headers: auth,
    body: JSON.stringify({ conversation, actor: String(req.user.id), vars: { user_name: req.user.firstName } }),
  });
  if (!g.ok) return res.status(502).end();
  const { token, expires_at } = await g.json();
  res.json({ token, expires_at });                    // never return the server secret
});
```

### 4. Web: embed the chat in an iframe

The host page's origin must be listed in `server.cors_origins`. That list is also the iframe's CSP `frame-ancestors`; without it, the browser refuses to frame `/embed`.

```html
<iframe id="hfy-chat" src="https://hfy.example.com/embed" title="Chat"
        style="border:0;width:100%;height:640px" allow="clipboard-write"></iframe>
<script>
  const HFY_ORIGIN = "https://hfy.example.com";
  const frame = document.getElementById("hfy-chat");

  window.addEventListener("message", async (e) => {
    if (e.origin !== HFY_ORIGIN || e.source !== frame.contentWindow) return;
    let msg; try { msg = typeof e.data === "string" ? JSON.parse(e.data) : e.data; } catch { return; }

    if (msg.type === "ready") {
      // optional per-viewer presentation
      frame.contentWindow.postMessage({ v: 1, type: "configure", lang: document.documentElement.lang || "en", theme: "auto" }, HFY_ORIGIN);
    }
    if (msg.type === "token-needed") {             // msg.reason: "initial" | "expired" | "unauthorized"
      const r = await fetch("/api/chat/grant", { method: "POST", credentials: "same-origin" });
      if (!r.ok) return;                           // e.g. logged out: the widget keeps waiting
      const { token, expires_at } = await r.json();
      frame.contentWindow.postMessage({ v: 1, type: "grant", token, expires_at }, HFY_ORIGIN);
    }
  });
</script>
```

The widget keeps the token in memory only. It asks again (`token-needed`) on expiry or a 401, then resumes the stream from its last event without gaps or duplicates. There is no URL configuration: `/embed` is always a bare URL, and you must never put a token in its query string. Keep answering `token-needed` even for renewable grants (see "Renewing a grant"): it comes with reason `initial` (first load), `expired` (the token ran out, or a renewal failed or hit its cap) or `unauthorized` (a request was refused). When you re-mint after `unauthorized`, revoke the previous grant's `id`: that ends its whole chain.

### 5. Mobile: embed the chat in a WebView

**Flutter apps: use the native hfy_flutter SDK (in development); do not embed /embed in a WebView.**

Load `https://<hfy>/embed` top-level in the WebView. Messages go out from the widget as a JSON string, and come back in by calling `window.hfy.receive(json)`. The widget picks the first outbound bridge that exists:

1. `window.HfyBridge.postMessage(str)`: Android `addJavascriptInterface(obj, "HfyBridge")` with a `@JavascriptInterface fun postMessage(s: String)`.
2. `window.webkit.messageHandlers.hfy.postMessage(str)`: iOS/macOS WKWebView, `userContentController.add(handler, name: "hfy")`.
3. `window.chrome.webview.postMessage(str)`: Windows WebView2.
4. `window.ReactNativeWebView.postMessage(str)`: React Native `react-native-webview`, via `onMessage`.

Inbound, from any shell: evaluate `window.hfy.receive(<json string or object>)`.

React Native:

```jsx
<WebView ref={ref} source={{ uri: "https://hfy.example.com/embed" }}
  onMessage={async (e) => {
    const msg = JSON.parse(e.nativeEvent.data);
    if (msg.type === "token-needed") {
      const g = await api.chatGrant();
      const out = JSON.stringify({ v: 1, type: "grant", token: g.token, expires_at: g.expires_at });
      ref.current.injectJavaScript(`window.hfy.receive(${JSON.stringify(out)}); true;`);
    }
  }} />
```

Android WebView needs the file-chooser callback (`onShowFileChooser` → Activity result) wired for document/attachment picking; this is documented here, not device-tested.

### 6. Custom web UI (JS client)

For a UI of your own instead of `/embed`. The page's origin must be in `server.cors_origins`.

```html
<script src="https://hfy.example.com/embed/client.js"></script>
<script>
  const client = new HfyClient({
    api: "https://hfy.example.com",
    onTokenNeeded: async (reason) => (await fetch("/api/chat/grant", { method: "POST" })).json(), // → {token, expires_at}
  });
  client.onEvent((envelope) => render(envelope));   // history and live events, deduped
  client.onState((state) => showConnection(state));
  await client.connect();
  await client.send("hello");                        // also: upload(file) → attachment, approve(callId, approve, result, answers), stop(), loadOlder(), close()
</script>
```

### 7. Presentation

Defaults live in YAML under `embed:`, are validated at startup and injected into the page:

```yaml
embed:
  lang: en              # omit = viewer's browser language
  dir: auto             # auto | ltr | rtl
  theme: auto           # light | dark | auto
  accent: "#0b7d71"     # hex
  density: comfortable  # comfortable | compact | spacious
  layout: bubbles       # bubbles | transcript | whatsapp (WhatsApp-style; an accent you set tints its own bubbles)
  tool_calls: summary   # hidden | summary (friendly one-line rows) | details (args and results, for developers); approval requests always show
```

Per-viewer overrides come only from a bridge message `{"v":1,"type":"configure", …}` with any of `lang`, `dir`, `theme`, `accent`, `density`, `layout`, `tool_calls` and `strings` (an object overriding UI strings). An `accent` there must be a six-digit hex such as `#e11d48`; anything else is ignored. Built-in strings are English and Arabic; time zone, language and direction auto-detect from the viewer. Arbitrary CSS/HTML and trust fields (token, API base, actor, agent, vars) are never client-configurable. The `/embed` page and its files are cached and compressed by hfy itself; there is nothing to configure.

What the UI does by itself: message bubbles with delivery ticks, streamed markdown, copy, photo/document attachments (when `files.dsn` is configured), approvals and `hfy.ask_user` questions as bottom sheets, tool cards (`_meta["hfy/ui"]`, see "Cards and questions from tools") with send/link/host buttons, Send turns into Stop while the viewer's own turn runs, history paging, reconnects, multi-viewer rooms, dark mode, RTL.

### 8. Give the agent tools (MCP)

```yaml
mcp:
  orders:
    type: remote                                  # Streamable HTTP
    url: https://internal.example.com/mcp
    headers: { Authorization: "Bearer ${ORDERS_MCP_TOKEN}" }
    approval: [refund, cancel_*]                  # these tools wait for the viewer's approval, for every agent
    approve_destructive: true                     # also wait on tools annotated destructiveHint: true
    ui:                                           # opt the server's tools into cards + tool-triggered questions
      image_hosts: ["img.shop.example", "*.imgshop.net"]  # https hosts allowed in cards' images
  files:
    type: local                                   # stdio command, started by hfy
    command: [mcp-filesystem, /data]
    environment: { LOG: "1" }                     # its only env besides PATH, HOME, USER, locale, TMPDIR, TZ
    timeout: 2m                                   # per tool call (default 2m)
agents:
  main:
    mcp: [orders]      # optional; omit = all servers, reached through hfy.tool_search (smaller prompt)
```

**Authorize inside the MCP server from `_meta["hfy/turn"].actor.ref`** (the person who started the turn); the model cannot change `_meta`. Return MCP `structuredContent` for data meant for client UIs; the model sees the text content.

Knowledge / RAG: hfy has no document store. Expose your docs, catalog or help center as an MCP search tool (filtered by `_meta.actor`), and the agent calls it when needed. For small, fixed knowledge, put it in the system prompt or pass it via grant `vars`.

## Rules

Do:
- Keep `HFY_SERVER_SECRET` on the server. Create conversations and grants only from the backend, after the app's own auth check.
- Store the conversation id per user/room in the app's database; create a new grant per widget request.
- Add every embedding origin (scheme + host + port, exactly as the browser sends it) to `server.cors_origins`.
- Set `context_window` to the model's real window, or somewhat below it.
- Check `e.origin` and `e.source` on iframe messages, and post to the exact hfy origin.
- Authorize tool calls in MCP servers from `_meta`, not from anything the model says.
- Run `hfy validate` after editing config.

Don't:
- Don't put tokens in URLs, cookies, localStorage or logs. Don't add URL parameters to `/embed`; they do nothing.
- Don't put secrets in grant `vars`: they reach the system prompt and every MCP server.
- Don't use one grant for two people. Each person in a room gets their own grant and actor.
- Don't build a document store inside hfy; expose knowledge as an MCP tool.
- Don't run one instance for several unrelated apps.

## Troubleshooting

- **Widget stays on "Connecting…"**: the host never answered `token-needed`. Check that the message listener runs, that the origin check uses the hfy origin, and that the grant endpoint returns `{token, expires_at}`.
- **iframe refuses to load (frame-ancestors)**: the host origin is missing from `server.cors_origins`.
- **401**: token expired, revoked or wrong instance. The widget asks for a new one automatically, so make sure the grant endpoint works for logged-in users.
- **403 on messages**: the grant's agent was removed from config. **403 on approvals**: only the turn's starter can decide.
- **400 on grant**: a required var is missing, or an undeclared var was sent.
- **The model never uses tools or asks in plain text**: the model is too small or lacks tool calling. Try a larger model, or name the tool in the system prompt.
- **400 "the agent cannot see images"**: set `settings.vision: true` on that agent (only for models that accept images).
- **Model ignores an image**: the provider cannot fetch its URL. Local model servers need a URL reachable from their host, so set `files.public_url` accordingly.
- **Uploads fail with S3**: the bucket CORS must allow PUT from the embed origins; `region=` is required in the dsn.
- **Startup error naming a line**: an unknown or misspelled config key.
