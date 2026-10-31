// The public API surface of the package: everything below imports ONLY
// package:hfy_flutter/hfy_flutter.dart — the barrel. If an app-needed symbol
// is missing from the barrel this file FAILS TO COMPILE (the intended
// compile-time guard), and if an internal name sneaks onto the barrel the
// forbidden-names scan below fails the run.
//
// No reflection (dart:mirrors does not exist in Flutter): the export contract
// is expressed as two plain name lists checked against the barrel's text —
//
//   * PUBLIC: every name that MUST stay exported (and is exercised below
//     for real),
//   * INTERNALS: names that must NEVER appear in the barrel — referencing
//     any of them through `package:hfy_flutter/hfy_flutter.dart` is a
//     compile error precisely because the barrel does not export them; the
//     scan keeps them out even when nothing happens to reference them.
import 'dart:io' show File;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/hfy_flutter.dart';

// The names the barrel promises (hfy_flutter.dart's export show clauses).
const List<String> kPublicNames = <String>[
  // client
  'HfyAttachmentRef',
  'HfyClient',
  'HfyClientConfig',
  'HfyClientOptions',
  'HfyUploadsConfig',
  'HfyConnectionState',
  // grants
  'HfyGrant',
  // exceptions
  'HfyAuthException',
  'HfyHttpException',
  'HfyNetworkException',
  // events
  'EVENT_TYPES',
  'HfyActor',
  'HfyEvent',
  'HfyProtocolException',
  // conversation fold
  'HfyAskOption',
  'HfyAskQuestion',
  'HfyAskState',
  'HfyAttachment',
  'HfyBlock',
  'HfyConversation',
  'HfyToolCalls',
  'HfyTurn',
  'HfyTz',
  'HfyViewer',
];

// Internals that an app must never see. "Cannot be referenced" in Dart is a
// compile-time property (a missing symbol is a compile error, not a runtime
// one, and dart:mirrors is unavailable) — so the guarantee is enforced by
// asserting these names appear nowhere in the barrel's exports.
const List<String> kInternalNames = <String>[
  // SSE framing + dedupe + watchdog: stream ids reach apps through HfyEvent,
  // never through the parser
  'HfySseParser',
  'HfySseMessage',
  'HfyIdleWatchdog',
  'HfyDedupe',
  // the REST layer: apps drive the client only
  'HfyApi',
  'HfyEventResponse',
  'HfyHistoryPage',
  'HfyTokenGate',
  // clock/timer typedefs: options accept plain lambdas
  'HfyTimerFactory',
  // the fold's pure helpers + state constants
  'hfySafeUrl',
  'hfyMapAttachments',
  'kHfyTerminalStates',
  'kHfyActiveStates',
  // fold/read shapes implemented privately in src (append-only here)
  'sanitizeServerError',
  'parseExpiryMs',
];

void main() {
  test('barrel exports exactly the public API (no internals)', () {
    // Kept relative to the package root so the scan reads the REAL barrel.
    // Only the export statements count: the doc comments above may name the
    // internal spellings to explain WHY they stay off the barrel. Export
    // clauses wrap across lines, so continuations are folded into one string.
    final barrel = File('lib/hfy_flutter.dart').readAsStringSync();
    var exportText = '';
    var inExport = false;
    for (final line in barrel.split('\n')) {
      if (line.startsWith('export ')) {
        inExport = true;
      }
      if (inExport) {
        exportText = '$exportText\n$line';
        // an export statement ends at its terminating ';'; wrapped
        // continuations (`show A, B,` on their own lines) keep accumulating
        if (line.contains(';')) inExport = false;
      }
    }
    for (final name in kPublicNames) {
      expect(
        exportText,
        contains(name),
        reason: '$name is part of the public API and must be exported',
      );
    }
    for (final name in kInternalNames) {
      expect(
        exportText,
        isNot(contains(name)),
        reason: '$name is internal and must stay OFF the barrel',
      );
    }
  });

  test('every public type is usable through the barrel alone', () {
    // client: construction is the usage (no network).
    final options = const HfyClientOptions();
    expect(options, isA<HfyClientOptions>());
    final client = HfyClient(
      api: Uri.parse('https://hfy.example'),
      tokenProvider: (_) async => <String, Object?>{
        'token': 't',
        'expires_at': '2026-12-31T00:00:00Z',
      },
      options: options,
    );
    expect(client.hasOlder, isFalse);
    client.close();

    // events + fold read types.
    final event = HfyEvent.fromJson(const <String, Object?>{
      'id': 'turn-1',
      'type': 'turn',
      'parent': null,
      'actor': <String, Object?>{'uuid': 'u1', 'type': 'external', 'ref': 'u1'},
      'v': 1,
      'data': <String, Object?>{'agent': 'main'},
      'created_at': '2026-01-02T10:00:01Z',
    });
    expect(event.id, 'turn-1');
    expect(EVENT_TYPES, containsAll(<String>['turn', 'message.text']));
    expect(event.isKnownType, isTrue);
    final actor = event.actor;
    expect(actor, isA<HfyActor>());
    expect(actor!.uuid, 'u1');

    // grants (fromResult is the callback contract).
    final grant = HfyGrant.fromResult('tok');
    expect(grant.token, 'tok');
    expect(grant.expiresAt, isNull);

    // the fold: drive a turn end-to-end and read the derived outlets.
    final conv = HfyConversation(
      viewer: HfyViewer.fromJson(
        const <String, Object?>{'uuid': 'u1', 'type': 'external', 'ref': 'u1'},
      ),
      toolCalls: HfyToolCalls.summary,
      tz: HfyTz.utc(),
      base: Uri.parse('https://embed.example/'),
    );
    conv.markLoaded();
    expect(conv.apply(event, history: false), isTrue);
    expect(conv.turns.single.id, 'turn-1');
    final HfyTurn turn = conv.turns.single;
    expect(turn.state, 'queued');
    expect(turn.blocks, isEmpty);
    expect(conv.pendingOwn, isEmpty);
    expect(conv.sheetOpen, isFalse);
    expect(conv.showActionPill, isFalse);
    expect(conv.showWelcome, isFalse);
    conv.minimizeSheet();
    conv.openSheet();
    conv.relayout();

    // HfyBlock state through an ask's option/question shapes.
    expect(HfyAskState.values, containsAll(<HfyAskState>[HfyAskState.pending]));
    expect(
      HfyAskState.values.map((s) => s.name),
      containsAll(<String>['pending', 'answered']),
    );
    const question = HfyAskQuestion(
      id: 'q1',
      text: 'Which?',
      type: 'single',
      required: true,
      placeholder: '',
      options: <HfyAskOption>[HfyAskOption(value: 'a', label: 'A')],
    );
    expect(question.options.single, isA<HfyAskOption>());
    const attachment = HfyAttachment(
      url: 'https://hfy.example/f',
      label: 'f',
      isImage: false,
      isLink: true,
      isPlain: false,
    );
    expect(attachment.isLink, isTrue);

    // exceptions carry the status contract (401 only is the auth subtype).
    const httpError = HfyHttpException(403, 'not your turn', body: null);
    const authError = HfyAuthException(401, 'no valid token', body: null);
    const netError = HfyNetworkException('connection refused');
    expect(httpError.status, 403);
    expect(authError.status, 401);
    expect(authError, isA<HfyHttpException>());
    expect(netError.message, contains('refused'));

    // uploads/config types (P-CFG-004's shape).
    final config = HfyClientConfig.fromWire(<String, Object?>{
      'types': <String>['text/plain'],
      'max_bytes': 11,
    });
    expect(config.uploads, isA<HfyUploadsConfig>());
    expect(config.uploads!.types.single, 'text/plain');
    final ref = HfyAttachmentRef(file: 'f', mime: 'text/plain');
    expect(ref.toWire(), isMap);
    expect(HfyConnectionState.values.length, greaterThan(3));
  });
}
