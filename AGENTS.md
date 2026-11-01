# AGENTS.md

hfy is a self-hosted backend and embeddable chat UI for AI conversations: one Go binary plus Postgres, configured by one YAML file.
The host app keeps its users and data; hfy runs the agent loop, calls MCP tools, stores every step as an event and streams replies over SSE.

`docs/reference.md` is the behavior reference. Its "Conventions" table lists each rule and what enforces it: a test, or review.

## Standing rules

- Bad text never reaches Postgres: strings go through `types.Clean` (drops NUL, replaces invalid UTF-8); JSON with a NUL or a lone `\u` surrogate is refused by `storable`, not cleaned.
- Errors a turn or a job records go through `types.ErrorText` (bounded, URL-scrubbed), never raw. Outbound trace rows do too: `recorder` (`internal/agent/sink.go`) applies `ErrorText` and `traceJSON` to every row, and the provider client removes its own API key from error text.
- Stack traces go to the log only (`internal/jobs/worker.go`), never into a turn, job, event or error text.
- Every new config section is strict-keyed (`checkKeys` in `internal/config/strict.go`), appears in `example.yaml` and in `docs/reference.md`. A removed key goes in `removedKeys` (`internal/config/strict.go`) with a tailored message.
- Breaking config changes get an "Upgrading to vN" section in `docs/reference.md`, as "Upgrading to v2" does.
- DB tests fail, not skip, under `HFY_TEST_REQUIRE_DB=1` (`dbtest.New`). Node tests do too: they start with `dbtest.NeedNode(t)`, which also reads `dbtest.Required()`. Use it for any new test that runs `node`.
- Golden fixtures change only through `-update-fixtures`, never by hand.
- `docs/llms.txt` is generated: edit `docs/llms.head.md` or `docs/reference.md`, run `make gen`, never hand-edit it. A plain `go test` fails with "docs/llms.txt is stale" when it differs. After a merge conflict in it, re-run `make gen` instead of merging by hand.
- Every new endpoint documents its status codes and gets a hostile-input table test (header forms, huge, negative and float numbers; see `internal/httpapi/grant_exchange_test.go`).
- Chain or row growth is bounded by a rate or size rule: the grant exchange allows one renewal per quarter of a token's lifetime (`TestExchangeIsRateLimited`).
- Secrets and tokens never go to logs, events or error text. A URL carries one only on the event stream (`?token=`, since EventSource cannot send headers) or in a signed file link.

## Layout

- `main.go`, `harness.go`: the CLI (`serve`, `queue:work`, `migrate`, `validate`, `config:example`) and process wiring.
- `internal/config`: YAML, strict keys, `${VAR}` expansion. `example.yaml` is embedded here.
- `internal/types`: shared vocabulary: events, payloads, the registry. Imports nothing internal.
- `internal/db`: all Postgres access. Embedded `migrations/` and `queries/`. `dbtest` gives tests a database.
- `internal/jobs`: the queue worker (`SKIP LOCKED`).
- `internal/svc`: conversation mutations shared by the API and the agent. Owns turn state.
- `internal/agent`: the one agent loop: main agent, sub-agents, tools, prompts, bounds.
- `internal/httpapi`: routes, SSE, and the embedded UI in `embed/`.
- `internal/provider`: OpenAI-compatible client and `mock`. `fakeprov` is a scripted fake for tests.
- `internal/mcp`: MCP client and manager. `builtins/` holds hfy's own tools. `fakemcp` is a fake stdio server for tests.
- `internal/files`, `internal/metrics`: blob storage (gocloud) and Prometheus.

## Layering

`arch_test.go` (`TestLayering`) holds the allowed-import table. It checks non-test files only.

- `types` imports nothing internal. `files`, `metrics` and the fakes import nothing internal.
- `httpapi` never imports `agent`: API and workers scale apart. `svc` never imports `agent` either.
- A new package must be added to the table, or the test fails.
- `TestTransactionsUseOnlyTheirConnection`: a function holding a `pgx.Tx` must not touch `.Pool`.
- `TestOnlySvcWritesTurnState`: only `svc` appends `turn.state` events.

## The gate

`make check` runs what `.github/workflows/ci.yml` runs:

1. `go mod tidy -diff`
2. `golangci-lint run ./...` (v2.15.0, built into `bin/`; rules in `.golangci.yml`)
3. `HFY_TEST_REQUIRE_DB=1 go test -race -count=1 ./...`

The Makefile pins `GOTOOLCHAIN=go1.27.3`, which matches `go.mod`.

CI's lint step is named "lint (formatting, errcheck, staticcheck, modernize, ...)", but `golangci-lint run` does not report formatting (checked). `make fmt-check` (`golangci-lint fmt --diff`) does, and it is not in CI.

You need Docker for `make db`, or any Postgres at `HFY_TEST_DB` (default `postgres://postgres:hfy@127.0.0.1:54330/hfy`). You also need `node` on PATH: the embed tests run the real `embed/ui.js` and `client.js` under node, and fail without it when `HFY_TEST_REQUIRE_DB=1`. CI uses postgres:17 and node 22.

If `make check` says "no test postgres", run `make db`. It starts the `hfy-testdb` container, or does nothing when it is already running.

When the tree has other people's uncommitted work, verify on a clean export of the commit:

```sh
d=$(mktemp -d) && git archive HEAD | tar -x -C "$d" && make -C "$d" check
```

## Tests

- DB tests call `dbtest.New(t, ...)`. Each gets its own migrated database, `hfy_t_<hex>`, dropped at cleanup. Without Postgres they skip locally, and with `HFY_TEST_REQUIRE_DB=1` (CI and `make test`) they fail.
- Tests use `fakeprov` and `fakemcp` instead of real model servers or MCP servers.
- Golden files: `clients/fixtures` (`expected/*.json`, `strings.json`, `markdown.json`) is generated from the real `embed/ui.js`. Regenerate with `go test ./internal/httpapi -run TestFixturesMatchJS -args -update-fixtures`. Markdown inputs stay hand-written; only their hashes and trees are regenerated. Hand edits fail the test.
- `TestTokenContract` parses the ``#### The `--hfy-*` tokens`` section of `docs/reference.md` (rows ``| `token` | layout |``) and reads `clients/fixtures/tokens.json` without changing it. Renaming that heading or reformatting those rows fails the test.
- JSON storage rules are judged by real Postgres: `TestStorableAgreesWithPostgres` in `internal/agent/persist_internal_test.go`. Add a row for each form you accept or refuse.
- `internal/db/plans_test.go` (`TestHotQueriesUseIndexes`) fails when hot queries fall back to sequential scans.

## Docs stay in sync

`docs/reference.md` is canonical. Update it for any change in behavior, a setting, an endpoint, an event or a limit.

- `docs/llms.txt`: the self-contained guide for coding agents, generated by `make gen` (`TestLLMsTxtIsFresh`, `-update-docs`) from `docs/llms.head.md` (intro, concepts, integration recipe, rules, troubleshooting) followed by `docs/reference.md` from its first `## ` heading. Facts live once, in `reference.md`; put a fact in the head only when it is a recipe step or a rule. Lines between `<!-- llms:skip -->` and `<!-- /llms:skip -->` (each on its own line, outside code fences, not nested) are left out of it: use them for maintainer-only or upgrade-only sections and for text that the head already says. A skipped section must not be linked to (`TestLLMsTxtShape` checks anchors), and a fact only a skipped section holds is not in `llms.txt`. A line that starts with `<!--` and names `llms:` must be exactly a marker, or the generator fails; a marker quoted in prose or code is fine. `.gitattributes` keeps `llms.txt` LF.
- `internal/config/example.yaml`: the commented starter that `hfy config:example` prints. Update it when a setting is added, renamed, or its default changes. It must load (`TestExampleParses`), and its commented agent controls must still load once uncommented (`TestExampleControlsUncommented`).
- `README.md`: the quick start and the CLI table. Update it when a command or flag changes. The Commands table in `docs/reference.md` changes with it.

`llms.txt` is checked by `TestLLMsTxtIsFresh` and `TestLLMsTxtShape` (`docs_test.go`). `TestDocYAMLFences` (`docs_yaml_test.go`) parses and validates every ```` ```yaml ```` fence in `README.md`, `docs/reference.md` and `docs/llms.head.md`: a partial snippet is merged into a minimal valid config first, so a fence needs only the keys it shows (and a `<provider>/<model>` it names gets a stub provider). A fence that is meant to be invalid or only illustrative takes a `# docs:skip-validate` comment line inside it. Other snippets (curl, HTML, JS) and keeping `README.md` and `reference.md` in sync with the code are review checks.

## Config

- Unknown keys stop startup and name their line and the known keys (`internal/config/strict.go`). A typo must never silently disable a setting. `hfy validate` checks a file without starting.
- A setting is a yaml-tagged field in `internal/config`; the tags are the known keys. Validate it in `config.go` and test it in `config_test.go`. Then update the docs above.
- `${VAR}` expands in string values, never in keys (`docs/reference.md`, Config; `TestEnvOnlyInValuesAndNumbers`).
- In `agents.<name>.settings.extra`, keys hfy owns are reserved in any letter case, and two keys that differ only by case are refused (`types.ExtraKeyReserved`, `config.go`).

## Migrations

- `internal/db/migrations/NNNNN_name.sql`, embedded and run by goose (`internal/db/store.go`; `hfy migrate up|down|status`). Each existing file has `-- +goose Up` and `-- +goose Down`. The highest is `00005_event_key.sql`, so the next free number is `00006`. `00005` runs outside a transaction (`-- +goose NO TRANSACTION`) so its index is built `CONCURRENTLY`; see its comments.
- One new file per schema change. Migrations stay compatible with the previous release: add first, remove in a later release (`docs/reference.md`, Conventions).
- Do not edit a migration that a tagged release has run (`git tag`). `76753b0` folded seven migrations into `00001_init.sql`; its message says nothing stored before needed keeping.
- No code creates embeddings tables any more: `00003_drop_embeddings.sql` dropped the old `embeddings_<dims>` ones.
- SQL lives in `internal/db/queries/*.sql`, embedded. For a change to a hot query, check `plans_test.go`.

## Hostile-input checklist

Use this for any change that parses, cleans, stores or replays text, JSON, numbers or ids: model output, tool output, HTTP bodies, or config values that reach the database. The code is in `internal/agent/bounds_rules.go` (`storable`, `storableText`, `clipName`) and `internal/types/types.go` (`Clean`).

1. NUL (`\x00`) in strings and object keys. Postgres jsonb refuses it. Drop it with `types.Clean`, or refuse it (`errNotText`).
2. Lone surrogate escapes (`\ud800`, `\udc00`) in values and keys. `hasLoneSurrogate` refuses them.
3. Invalid UTF-8 and a BOM. `types.Clean` replaces invalid UTF-8. A BOM fails `json.Valid`.
4. Number blow-ups. `1e100000` is 10 bytes in and 100,003 bytes out in jsonb. `writtenLen` caps the written-out digits at 1000.
5. Duplicate keys. Postgres parses every value and keeps the last one. `storable` walks every token, so a bad value under a repeated key is still caught.
6. Nesting. Depth is capped at 64 (`maxDepth`) by a token loop, not recursion. Check the wrappers: a payload adds one or two levels (`TestStorableDepthFitsEveryWrapper`).
7. Size after re-serialization. Postgres writes a space after every `,` and `:`, and writes numbers out in full. `storable` counts both against the limit it is given.
8. Case. Reserved keys compare with `EqualFold` (`types.ExtraKeyReserved`). Host matching lowercases both sides (`svc.HostAllowed`).
9. Id collisions. Call ids must be unique per turn and across the conversation. `agent/loop.go` replaces a clashing or unstorable id with `call_<step>_<index>`. Question ids are de-duplicated in `svc/ask.go`.
10. Every string that reaches jsonb. Tool names and call ids go through `storableText` or `clipName`. Model and tool output go through `types.Clean` (`agent/sink.go`). Request input goes through `types.Clean` (`httpapi/post_message.go`, `approvals.go`, `grants.go`). A new path needs the same treatment.
11. Add a row to `TestStorableAgreesWithPostgres`. Postgres has the final word on the JSON rules.

## Commits

- Short subject, usually `Area: what changed` (`Agent loop:`, `Docs:`, `Embed:`), or an imperative (`Add AGENTS.md and make check`).
- Body: the why, when the diff does not show it.
- Trailers: the last 30 commits have none, and older ones carry `Co-Authored-By`. Add one only when asked.
- Stage explicit paths with `git add <paths>`, never `git add -A`. The tree often has other people's uncommitted work.

## Never commit

- Ignored by `.gitignore`: `/bin/` (build output, including the lint binary), `*.local.yaml`, `agent.yaml`, `HANDOFF.md`, `.idea/`. Do not force-add them.
- Secrets. Config reads them from the environment (`${HFY_SERVER_SECRET}` in `example.yaml`). Keep real keys and tokens out of YAML, docs and tests.
- Generated files only as regenerated output (see Tests). Never hand-edit `clients/fixtures/expected/*.json` or `strings.json`.
