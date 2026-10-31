// Token lifecycle tests: the refresh-skew timer (P-CLIENT-024/025), the
// 401 -> one refresh -> one retry rule (P-CLIENT-021), single-flight sharing
// and onTokenNeeded's contract (P-CLIENT-017). All timings run on FakeTimers
// (PARITY.md §11-2).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyProtocolException;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  setUp(() async {
    server = await FakeHfyServer.start();
  });
  tearDown(() => server.destroy());

  test('P-CLIENT-024/005: refresh fires BEFORE expiry with the skew '
      '(default 30s, delay = remaining − min(skew, remaining/2))', () async {
    final kit = await ClientKit.open(server, ttlMs: 20000);
    // Minted expiry = now+20000, skew 30s: min(30000, 10000) -> delay 10000 ms.
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.state, HfyConnectionState.live);
    final refresh = kit.timers.activeOneOrNull(const Duration(milliseconds: 10000));
    expect(refresh, isNotNull,
        reason: 'one refresh timer scheduled for T+10s = 20s expiry - 10s head');
    // The idle watchdog's 30s timer is separate and still armed.
    expect(kit.timers.activeWithDelay(const Duration(seconds: 30)), hasLength(1));
  });

  test('P-CLIENT-024: an expired grant refreshes at the 1000ms floor', () async {
    final kit = await ClientKit.open(server);
    kit.tokens.queueGrant('tok-gone', expiresAtMs: -10);
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.timers.activeOneOrNull(const Duration(milliseconds: 1000)),
        isNotNull, reason: 'max(remaining − head, 1000) floors at 1000');
  });

  test('P-CLIENT-024: no expiry -> NO proactive refresh timer', () async {
    final kit = await ClientKit.open(server);
    kit.tokens.queueMap({'token': 'tok-noexpiry'});
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.timers.active, hasLength(1),
        reason: 'only the idle watchdog (30s) stays armed');
    expect(kit.timers.active.single.delay, const Duration(seconds: 30));
  });

  test('P-CLIENT-025: the refresh timer mints reason `expired`, reopens the '
      'stream and returns to live', () async {
    final kit = await ClientKit.open(server, ttlMs: 20000);
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    kit.tokens.queueGrant('tok-2', expiresAtMs: 60000);
    kit.timers.now = 10000;
    final refresh = kit.timers.activeOneOrNull(const Duration(milliseconds: 10000))!;
    refresh.run();
    await settle();
    // The refresh ran: provider reason `expired`, one more SSE connection,
    // state live again.
    expect(kit.tokens.reasons.last, 'expired');
    expect(server.sseOpenCount, 2, reason: 'stale stream closed, new opened');
    expect(kit.state, HfyConnectionState.live);
    // The stale session must disappear client-side; the zombie socket itself
    // is invisible to a dart:io server (probes documented in the fake).
    await settle(50);
    expect(server.sseConnections[0].closed, isFalse,
        reason: 'dart:io: a force-closed client socket stays invisible');
  });

  test('P-CLIENT-021 + P-CLIENT-022: 401 -> one refresh (reason '
      '`unauthorized`) -> one retry with the new token; retry succeeds',
      () async {
    final kit = await ClientKit.open(server);
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    // Reject the first API request (the history call) once.
    server.schedule401();
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.tokens.callsIn('initial'), 1);
    expect(kit.tokens.callsIn('unauthorized'), 1);
    // Two history requests happened: the 401 then the retried one.
    final historyCalls =
        server.requests.where((r) => r.path.contains('history')).length;
    expect(historyCalls, 2);
    // The retry carried the refreshed (minted-on-demand) token tok-2.
    final retry = server.requestWhere(
        (r) => r.path.contains('history') && r.authorization.endsWith('tok-2'));
    expect(retry, isNotNull);
    expect(kit.state, HfyConnectionState.live);
  });

  test('P-CLIENT-021: a second 401 surfaces (retries disabled, no second '
      'refresh)', () async {
    final kit = await ClientKit.open(server);
    server.schedule401();
    server.schedule401();
    // Both history attempts fail; the client surfaced the typed error.
    await expectLater(kit.client.connect(), throwsA(isA<HfyAuthException>()));
    expect(kit.state, HfyConnectionState.error);
    expect(kit.tokens.callsIn('unauthorized'), 1,
        reason: 'exactly one refresh despite two 401s');
    final historyCalls =
        server.requests.where((r) => r.path.contains('history')).length;
    expect(historyCalls, 2, reason: 'one attempt, one retry, nothing more');
  });

  test('native hardening: single-flight — two racing send() calls share ONE '
      'initial token fetch', () async {
    final kit = await ClientKit.open(server);
    await Future.wait(<Future<void>>[
      kit.client.send('a'),
      kit.client.send('b'),
    ]);
    expect(kit.tokens.reasons, ['initial'],
        reason: 'one provider call shared by both racing requests');
    final bearer = <String>{for (final r in server.requests) r.authorization};
    expect(bearer.length, 1);
  });

  test('P-CLIENT-017: an empty token result throws the exact client.js '
      'error', () async {
    final kit = await ClientKit.open(server);
    kit.tokens.queueNothing();
    server.historyPages[null] = historyPage();
    await expectLater(
        kit.client.connect(), throwsA(isA<HfyProtocolException>()));
    expect(kit.state, HfyConnectionState.error);
  });
}
