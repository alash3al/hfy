# hfy_flutter

Native Flutter SDK for [hfy](https://github.com/alash3al/hfy): conversation
client + embed UI in one package. It must reproduce the embedded web UI
(`internal/httpapi/embed/{client.js,ui.js,ui.css,index.html}`) with no
functional or visual difference except where the platform requires — see
[`clients/flutter/PARITY.md`](../PARITY.md), the checklist every later phase
is built and reviewed against.

**Status: in development.** Phase 1 has landed the pure, fully tested
building blocks only: the wire event model (`HfyEvent`, `HfyGrant`, the
`EVENT_TYPES` list), the SSE frame parser with its idle-stream watchdog, and
the envelope dedupe. No networking/HTTP/UI yet.

## Development

All Flutter/Dart commands go through [fvm](https://fvm.app) — never a bare
`flutter`/`dart`. The version is pinned in `.fvmrc` (3.35.2):

```sh
cd clients/flutter/hfy_flutter
fvm flutter analyze
fvm flutter test
```

## Pinned rules (from PARITY.md)

- fvm only; pin via `cat .fvmrc`.
- State management: Flutter built-ins only (ChangeNotifier/ValueNotifier/
  StreamBuilder), later phases.
- HTTP: `dio`. No other third-party packages (dev-only `flutter_lints`).

## Layout

```
lib/hfy_flutter.dart        public API (the only stable surface)
lib/src/model/event.dart    wire envelope {id,type,parent,actor,v,data,created_at}
lib/src/client/token.dart   token grant + tolerant expiry parsing (client.js parseExpiryMs)
lib/src/client/sse.dart     SSE frame parser + no-bytes-30s idle watchdog (P-NAT-002)
lib/src/client/dedupe.dart  envelope dedupe / lastEventId cursor (client.js #fire)
test/                       unit tests, named with the PARITY IDs they cover
```
