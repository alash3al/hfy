// The PROJECTION (clients/fixtures/expected/*.json schema) for the folded
// Dart state — a field-for-field port of fixtures_test.go's JS `projection()`
// (fixtures_test.go:88-153), which follows clients/fixtures/README.md §
// "expected/<name>.json — the PROJECTION".
//
// `undefined` in the JS fold becomes an ABSENT key here (same as
// JSON.stringify dropping it); `null` values stay present. Key order is
// irrelevant to the comparisons; array order is exact.

import 'dart:convert' show jsonEncode;

import 'package:hfy_flutter/src/model/ask.dart' show hfyAskStateName;
import 'package:hfy_flutter/src/model/fold.dart' show HfyConversation;
import 'package:hfy_flutter/src/model/state.dart';
import 'package:hfy_flutter/src/model/values.dart' show hfyTruthy;

/// The wire's `tool_result.data.structured` per call id, collected the way
/// fixtures_test.go:166-173 does — from the RAW event envelopes of the main
/// sequence AND every page, last write wins, before any dedupe.
Map<String, Object?> wireStructuredOf(Map<String, dynamic> def) {
  final wire = <String, Object?>{};
  for (final e in eventsOf(def)) {
    final data = e['data'] is Map<String, Object?>
        ? e['data'] as Map<String, Object?>
        : const <String, Object?>{};
    if (e['type'] == 'tool_result' && data['structured'] != null) {
      final cid = data['call_id'];
      // JS object keys are strings; a numeric call id coerces the same way
      // the fold's block callId does. Fixtures always send strings.
      wire[cid is String ? cid : '$cid'] = data['structured'];
    }
  }
  return wire;
}

/// The story's raw envelopes: `events`, then every page's `events`.
List<Map<String, Object?>> eventsOf(Map<String, dynamic> def) {
  final out = <Map<String, Object?>>[];
  void add(Object? list) {
    if (list is! List) return;
    for (final e in list) {
      if (e is Map<String, Object?>) out.add(e);
    }
  }

  add(def['events']);
  for (final p in _pagesOf(def)) {
    add(p['events']);
  }
  return out;
}

List<Map<String, Object?>> _pagesOf(Map<String, dynamic> def) {
  final pages = def['pages'];
  if (pages is! List) return const <Map<String, Object?>>[];
  return <Map<String, Object?>>[
    for (final p in pages)
      if (p is Map<String, Object?>) p,
  ];
}

/// `projection(def, c, wireStructured, hasOlder, lastEventId)` — one scenario.
Map<String, dynamic> projectFixtureStory({
  required Map<String, dynamic> def,
  required HfyConversation c,
  required Map<String, Object?> wireStructured,
  required bool hasOlder,
  required String? lastEventId,
}) {
  Map<String, dynamic> blockOf(HfyBlock b) {
    final base = <String, dynamic>{
      'id': b.id,
      'kind': b.kind,
      'iso': b.iso,
      // grouping (relayout): day separator and group head, derived from the
      // previous VISIBLE block of any kind — not from hidden tool rows
      'showDay': b.showDay,
      'showHead': b.showHead,
    };
    if (b.isText) {
      base['role'] = b.isAgent ? 'agent' : 'user';
      base['ref'] = b.ref;
      base['text'] = b.text;
      base['streaming'] = b.streaming;
      // the fold's own attachment split (setAtts → mapAtts): every field the
      // template reads, urls resolved by the safe-url rule against the base
      if (b.images.isNotEmpty || b.files.isNotEmpty) {
        base['attachments'] = <String, dynamic>{
          'images': [for (final a in b.images) attView(a)],
          'files': [for (final a in b.files) attView(a)],
        };
      }
      return base;
    }
    if (b.isReasoning) {
      base['text'] = b.text;
      base['streaming'] = b.streaming;
      return base;
    }
    base['callId'] = b.callId;
    base['name'] = b.name;
    base['status'] = b.status;
    base['args'] =
        b.args; // argsRaw == null ? null : argsRaw (collapsed already)
    base['content'] = b.content;
    base['isAsk'] = b.isAsk;
    if (b.questions.isNotEmpty) {
      base['questions'] = [
        for (final q in b.questions)
          <String, dynamic>{
            'id': q.id,
            'text': q.text,
            'type': q.type,
            'required': q.required,
            'placeholder': q.placeholder,
            'options': [
              for (final o in q.options)
                <String, dynamic>{'value': o.value, 'label': o.label},
            ],
          },
      ];
    }
    if (hfyTruthy(b.answers)) base['answers'] = b.answers;
    if (b.questions.isNotEmpty) {
      base['askState'] = b.askState == null ? '' : hfyAskStateName(b.askState!);
    }
    if (b.decision.isNotEmpty) base['decision'] = b.decision;
    if (b.declineReason.isNotEmpty) base['declineReason'] = b.declineReason;
    base['stopped'] = b.stopped;
    base['showApproval'] = b.showApproval;
    // tool visibility (P-FOLD-024): `hidden` mirrors the row's is-hidden
    base['showSummary'] = b.showSummary;
    base['showDetails'] = b.showDetails;
    base['hidden'] = b.hidden;
    base['showDecided'] = b.showDecided;
    base['showPendingOwn'] = b.showPendingOwn;
    base['showPendingOther'] = b.showPendingOther;
    if (b.card != null) base['card'] = b.card;
    base['cardButtonsLive'] = b.cardButtonsLive;
    // the fold keeps only a pretty-printed copy of structured data; the
    // projection records the wire's JSON (fixtures_test.go:122)
    final wire = wireStructured[b.callId];
    if (wire != null) base['structured'] = wire;
    return base;
  }

  Map<String, dynamic> turnOf(HfyTurn t) {
    return <String, dynamic>{
      'id': t.id,
      'agent': t.agent, // JS `agent: t.agent` — d.agent || old, usually ""
      'state': t.state,
      if (hfyTruthy(t.error)) 'error': t.error, // t.error || undefined
      'own': t.own,
      'showTyping': t.showTyping,
      'blocks': [for (final b in t.blocks) blockOf(b)],
    };
  }

  return <String, dynamic>{
    'name': def['name'],
    'path': def['path'],
    'viewer': hfyTruthy(def['viewer'])
        ? def['viewer']
        : null, // echo `def.viewer ‖ null`
    'config': <String, dynamic>{'tool_calls': c.toolCalls.name},
    'lastEventId': lastEventId,
    'hasOlder': hasOlder,
    'showWelcome': c.showWelcome,
    'pendingOwn': c.pendingOwn,
    'sheetOpen': c.sheetOpen,
    'showActionPill': c.showActionPill,
    'turns': [for (final t in c.turns) turnOf(t)],
  };
}

/// `attView` (fixtures_test.go:85): the fold's `k` row key is deliberately
/// not part of the projection (README text-block row).
Map<String, dynamic> attView(HfyAttachment a) => <String, dynamic>{
  'url': a.url,
  'label': a.label,
  'isImage': a.isImage,
  'isLink': a.isLink,
  'isPlain': a.isPlain,
};

/// Deep JSON equality with object keys order-insensitive and exact array
/// order. Returns the first differing path, or null.
String? firstJsonDifference(Object? got, Object? want, [String path = '']) {
  if (got is Map && want is Map) {
    final keys = <Object?>{...got.keys, ...want.keys};
    for (final key in keys) {
      final inGot = got.containsKey(key);
      final inWant = want.containsKey(key);
      if (!inGot) return '$path/$key: absent in got, want=${_short(want[key])}';
      if (!inWant) return '$path/$key: got=${_short(got[key])}, absent in want';
      final diff = firstJsonDifference(got[key], want[key], '$path/$key');
      if (diff != null) return diff;
    }
    return null;
  }
  if (got is List && want is List) {
    if (got.length != want.length) {
      return '$path: length got=${got.length} want=${want.length}';
    }
    for (var i = 0; i < got.length; i++) {
      final diff = firstJsonDifference(got[i], want[i], '$path[$i]');
      if (diff != null) return diff;
    }
    return null;
  }
  if (got is num && want is num && got == want) return null;
  if (got == want) return null;
  return '$path: got=${_short(got)} want=${_short(want)}';
}

String _short(Object? v) {
  if (v is String) return '"$v"';
  return jsonEncode(v);
}
