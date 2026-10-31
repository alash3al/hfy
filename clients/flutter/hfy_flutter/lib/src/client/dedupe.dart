// Envelope dedupe + the http-stream cursor (PARITY.md P-CLIENT-015, and the
// fixtures contract "Duplicate delivery" in clients/fixtures/README.md).
//
// Rule, verbatim from clients/fixtures/README.md and HfyClient's #fire
// (internal/httpapi/embed/client.js:165-173):
//   1. an envelope with `id != null` is delivered AT MOST ONCE: a per-client
//      seen-set tracks every id, and a re-delivered envelope is dropped
//      entirely — before it can fire, move the cursor, or fold;
//   2. only a LIVE delivery (not history) updates lastEventId; an envelope
//      advances the cursor exactly once, on its first (necessarily live)
//      delivery. liveAfter/history deliveries never touch the cursor, and
//      `connect()` PINS live_after into it (client.js:96-97).
//
// Envelopes without an id (client.js:167) always deliver and never touch
// the seen-set or the cursor. After [close] nothing delivers (client.js's
// `#closed` guard) — the same rule that keeps close()d clients silent.
class HfyDedupe {
  // shortcut: unbounded seen-set, ~1 entry per envelope id for the whole
  // session (client.js:43-44, PARITY.md P-CLIENT-016) — revisit if profiling
  // demands compaction.
  final Set<String> _seen = <String>{};

  String? _lastEventId;
  bool _closed = false;

  bool get closed => _closed;

  /// The http-stream cursor: `live_after` when pinned, else the newest live
  /// envelope id delivered (client.js #lastEventId).
  String? get lastEventId => _lastEventId;

  /// connect(): live_after wins over any last fired id — it is the server's
  /// end mark of the history page (client.js:96-97, the fixtures rule
  /// "history pins liveAfter").
  void pinLiveAfter(String id) => _lastEventId = id;

  /// Feed one envelope's id here. Returns true exactly when the envelope
  /// may fire; a false return drops it before anything else — before it
  /// folds, or advances the cursor. `isHistory` envelopes are seeded into
  /// the seen-set but never move the cursor (client.js #fire(envelope, true),
  /// the loadOlder path). `close()`d dedupes refuse everything.
  bool claim(String? id, {required bool isHistory}) {
    if (_closed) return false;
    if (id == null) return true;
    if (_seen.contains(id)) return false;
    _seen.add(id);
    if (!isHistory) _lastEventId = id;
    return true;
  }

  /// After this: no delivery ever fires again (client.js #closed guard,
  /// P-CLIENT-014's "after closed no events fire").
  void close() => _closed = true;
}
