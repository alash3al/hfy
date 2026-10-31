// The Dart fixture harness: drives the HfyConversation fold exactly the way
// fixtures_test.go's JS harness drives the real window.hfyEmbed() fold
// (fixtures_test.go:161-219) for one scenario:
//
//   - path 'live' folds every event with isHistory FALSE unless the event
//     carries history:true (the scenario's leading history run); path
//     'history' folds everything as connect()'s page;
//   - duplicate delivery + the cursor through the real HfyDedupe (client.js
//     #fire semantics, clients/fixtures/README.md "Duplicate delivery"): an
//     id is delivered at most once; only live deliveries move lastEventId,
//     and liveAfter is PINNED right after the history run;
//   - pages[] (newest first) go through the fold's REAL loadOlder merge,
//     mergeOlderPage, one page per call while hasOlder holds (client.js
//     loadOlder);
//   - `minimize` runs the real minimizeSheet() before the final relayout +
//     syncDerived pass;
//   - the projection comes from projection.dart and is compared against
//     clients/fixtures/expected/<name>.json.
//
// The fold runs with HfyTz.utc() injected: the production default is the
// DEVICE zone (HfyTz.local), but the shared fixture contract pins UTC — the
// JS harness does the same by running node with TZ=UTC (README's header
// rules) — so grouping-gaps scenarios stay identical on every machine.

import 'package:hfy_flutter/src/client/dedupe.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyEvent;
import 'package:hfy_flutter/src/model/fold.dart' show HfyConversation;
import 'package:hfy_flutter/src/model/state.dart' show HfyTz, HfyToolCalls, HfyViewer;

import 'projection.dart';

/// The page location the JS harness hands ui.js through its `location` stub
/// (fixtures_test.go:67-71): the safe-url rule resolves the server's
/// relative /v1/files/… urls against it (uploads.go:220-228).
final Uri embedExample = Uri.parse('https://embed.example/');

/// Runs one fixture scenario to its final projection. With [batch] the main
/// sequence folds through applyAll() instead of per-event apply() — a
/// contract test asserts both land on the identical projection (the
/// relayout()/syncDerived() passes are pure recomputes); everything else
/// (dedupe, cursor pinning, pages, minimize) is byte-identical in both
/// variants.
Map<String, dynamic> runScenario(Map<String, dynamic> def, {bool batch = false}) {
  final conv = HfyConversation(
    viewer: HfyViewer.fromJson(def['viewer']),
    toolCalls: _modeOf(def),
    base: embedExample,
    tz: const HfyTz.utc(), // the pinned zone (see the header note above)
  );
  conv.markLoaded(); // the real client has connected by the time events arrive
  final wire = wireStructuredOf(def);
  // Client-like delivery, mirrors client.js #fire plus the harness's fire():
  // an envelope id is delivered ONCE (the seen-set lives in HfyDedupe), and
  // only live deliveries move lastEventId.
  final dedupe = HfyDedupe();
  final events = asEnvelopeList(def['events']);
  final pathIsHistory = def['path'] == 'history';
  final delivered = <({HfyEvent e, bool isH})>[];
  for (var i = 0; i < events.length; i++) {
    final e = events[i];
    // history-flagged events: the leading run's page
    final isH = pathIsHistory || e.json['history'] == true;
    if (dedupe.claim(e.id, isHistory: isH)) delivered.add((e: e, isH: isH));
    // connect() pins the page's end mark as the stream cursor right after
    // the history page (fixtures_test.go:205-209) so live events move it on.
    final nextIsH =
        i + 1 < events.length &&
        (pathIsHistory || events[i + 1].json['history'] == true);
    if (isH && !nextIsH && def['liveAfter'] != null) {
      final live = def['liveAfter'];
      dedupe.pinLiveAfter(live is String ? live : '$live');
    }
  }
  if (batch) {
    conv.applyAll([for (final d in delivered) d.e], history: false);
  } else {
    for (final d in delivered) {
      conv.apply(d.e, history: d.isH);
    }
  }
  // pages[] through the REAL loadOlder merge — one page per call while
  // hasOlder holds (the mock client's loop, fixtures_test.go:210-213).
  var hasOlder = def['hasOlder'] == true;
  final pages = _pagesOf(def);
  if (pages.isNotEmpty) {
    var pageIdx = 0;
    while (hasOlder) {
      final page = pageIdx < pages.length
          ? pages[pageIdx++] // the mock's `p = pages[pageIdx++]`
          : null;
      final pageEvents = <HfyEvent>[];
      for (final e in asEnvelopeList(page?['events'])) {
        if (dedupe.claim(e.id, isHistory: true)) pageEvents.add(e);
      }
      conv.mergeOlderPage(pageEvents, hasMore: page != null && page['next'] != null);
      hasOlder = page != null && page['next'] != null;
    }
  }
  // minimize is the user dragging the sheet down (fixtures_test.go:216).
  if (def['minimize'] == true) {
    conv.minimizeSheet();
    conv.relayout();
    conv.syncDerived();
  }
  conv.relayout();
  conv.syncDerived();
  return projectFixtureStory(
    def: def,
    c: conv,
    wireStructured: wire,
    hasOlder: hasOlder, // the mock client's hasOlder, not the component's
    lastEventId: dedupe.lastEventId,
  );
}

HfyToolCalls _modeOf(Map<String, dynamic> def) {
  final cfg = def['config'];
  final mode = cfg is Map<String, Object?> ? cfg['tool_calls'] : null;
  for (final m in HfyToolCalls.values) {
    if (m.name == mode) return m;
  }
  return HfyToolCalls.summary; // P-CFG-003's fallback
}

List<Map<String, Object?>> _pagesOf(Map<String, dynamic> def) {
  final pages = def['pages'];
  if (pages is! List) return const <Map<String, Object?>>[];
  return <Map<String, Object?>>[
    for (final p in pages)
      if (p is Map<String, Object?>) p,
  ];
}

/// Decodes the story's envelope lists through the typed model, exactly once
/// per entry (the JS harness parsed them too).
List<HfyEvent> asEnvelopeList(Object? list) {
  if (list is! List) return const <HfyEvent>[];
  return <HfyEvent>[
    for (final e in list)
      if (e is Map<String, Object?>) HfyEvent.fromJson(e),
  ];
}
