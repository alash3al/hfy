# hfy

**Put an AI assistant inside your app. Skip the plumbing.**

hfy is a backend and chat window for AI conversations that you run yourself.
Your app keeps its users and its data. hfy runs the model, calls your tools,
saves every conversation and gives your users a chat that feels native on your
website and in your mobile or desktop app.

[Website](https://alash3al.github.io/hfy/) ·
[Guide for coding agents](https://alash3al.github.io/hfy/llms.txt) ·
[Full reference](docs/reference.md)

## What you get

- **A ready-made chat window** for web pages and mobile/desktop WebViews. It shows replies as they're written and handles photos and files, dark mode and right-to-left languages.
- **Any model**: OpenAI, OpenRouter, Ollama or anything that speaks the OpenAI API.
- **Your tools** through MCP. Every call tells your tool server who is asking.
- **Nothing gets lost**: refreshes, restarts and crashes don't drop a message.
- **Questions instead of guesses**: the assistant can ask the user a short form.
- **Approval only where it matters**: risky actions wait for a tap, everything else just runs.
- **Rooms**: several people and assistants in one conversation.
- **One binary and Postgres.** That's the whole stack.

## Install

Pick one:

```sh
# 1. Download a binary (Linux, macOS, Windows · amd64, arm64)
#    https://github.com/alash3al/hfy/releases/latest
curl -sSL https://github.com/alash3al/hfy/releases/latest/download/hfy_linux_amd64.tar.gz | tar xz

# 2. Docker
docker pull ghcr.io/alash3al/hfy

# 3. Go
go install github.com/alash3al/hfy@latest
```

## Run it

You need Postgres with pgvector. One line starts one:

```sh
docker run -d --name hfy-db -p 5432:5432 -e POSTGRES_PASSWORD=hfy -e POSTGRES_DB=hfy pgvector/pgvector:pg17
```

Then:

```sh
hfy config:example > hfy.yaml        # a starter config, every option explained
export HFY_SERVER_SECRET=$(openssl rand -hex 24)
export DATABASE_URL=postgres://postgres:hfy@localhost:5432/hfy
hfy validate                         # checks the config
hfy serve --work                     # http://localhost:8080
```

Or run hfy and Postgres together from a clone: `docker compose up` (it reads `./hfy.yaml`).

The parts of `hfy.yaml` you'll actually touch:

```yaml
providers:
  openai:
    url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}

agent:
  model: openai/gpt-4o-mini
  settings: { context_window: 128000 }   # required: the model's context size
  system: You are the support assistant for Acme.
```

No model yet? Use the built-in fake one. It replies instantly and needs no network:

```yaml
providers:
  mock: { type: mock, mock: { reply: "Hello from mock" } }
agent:
  model: mock/demo
  settings: { context_window: 8192 }
```

## Put the chat in your app

**1. Your backend hands out a pass.** Do this after your own login check. Keep
the server secret on the server.

```sh
# once per conversation; store the id
curl -XPOST localhost:8080/v1/conversations -H "Authorization: Bearer $HFY_SERVER_SECRET"
# {"id":"7f3c…"}

# every time someone opens the chat
curl -XPOST localhost:8080/v1/grants -H "Authorization: Bearer $HFY_SERVER_SECRET" \
  -d '{"conversation":"7f3c…","actor":"user-42"}'
# {"token":"hfy_…","expires_at":"…"}
```

**2. Your page shows the chat.** Add your site's origin to `server.cors_origins`
in `hfy.yaml`, then:

```html
<iframe id="chat" src="http://localhost:8080/embed" style="border:0;width:100%;height:600px"></iframe>
<script>
  const HFY = "http://localhost:8080";
  addEventListener("message", async (e) => {
    if (e.origin !== HFY) return;
    const msg = JSON.parse(e.data);
    if (msg.type !== "token-needed") return;
    const pass = await fetch("/my-api/chat-pass").then((r) => r.json()); // step 1, from your backend
    chat.contentWindow.postMessage({ v: 1, type: "grant", token: pass.token, expires_at: pass.expires_at }, HFY);
  });
</script>
```

That's it. When a pass expires, the chat asks for a new one by itself.

For **mobile and desktop apps**, open `/embed` in a WebView instead; see
[WebViews](docs/llms.txt) for Flutter, React Native, iOS and Android. To build
your own UI, use `/embed/client.js` or the HTTP API.

## Give it your tools

Tools come from [MCP](https://modelcontextprotocol.io) servers:

```yaml
mcp:
  orders:
    type: remote
    url: https://internal.example.com/mcp
    headers: { Authorization: "Bearer ${ORDERS_TOKEN}" }
    approval: [refund]          # the user must confirm refunds
```

Every call includes who is asking (`_meta`), so your server decides what that
person may see or do.

## Let your coding agent do it

```
Read https://alash3al.github.io/hfy/llms.txt and add an hfy chat to our app.
```

## Commands

| | |
|---|---|
| `hfy serve [--work]` | the API; `--work` also runs the workers |
| `hfy queue:work` | workers only, to scale them separately |
| `hfy validate` | check the config |
| `hfy migrate up\|down\|status` | database migrations (also run on start) |
| `hfy config:example` | print the starter config |
| `hfy version` | print the version |

## Learn more

- [docs/reference.md](docs/reference.md): every setting, API endpoint, event and behavior, plus development notes
- [docs/llms.txt](docs/llms.txt): the complete integration guide for AI coding agents

## License

[MIT](LICENSE)
