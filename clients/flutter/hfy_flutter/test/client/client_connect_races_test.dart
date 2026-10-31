// The connect/pause/resume races the review items pin down, all driven
// deterministically (Dart async runs a connect() body synchronously up to
// its first await, so pause()/resume() issued from the test body land
// MID-BOOT without any wall-clock):
//   - item 7: pause() during the boot still pins live_after; resume() then
//     reopens FROM the pin, never from the all-zero id;
//   - item 8: racing connect() callers share ONE boot future; resume()
//     during a boot defers to it (no double history, no double stream); a
//     failed boot shares its error and frees the slot for the next one;
//   - item 9 (revised): pause() before the first connect() is an intent (see
//     client_pause_intent_test.dart); close() and dispose() release the
//     client's resources.
import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart';

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  test('item 7: pause() during the boot still pins live_after; resume() '
      'reopens FROM the pin (never the all-zero id)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final boot = kit.client.connect();
    kit.client.pause(); // lands during the in-flight boot
    await boot; // the boot reached its paused-check and slept: still a
    // NORMAL completion (review item 7 only moves the PIN).
    expect(kit.state, HfyConnectionState.paused);
    expect(server.sseOpenCount, 0, reason: 'no stream while paused');
    expect(kit.client.lastEventId, 'la-0',
        reason: 'the boot pinned live_after BEFORE the paused check');
    expect(kit.timers.hasActiveTimers, isFalse);

    kit.client.resume();
    await until(() => server.sseOpenCount == 1, timeoutMs: 5000);
    final req = server.sseConnections.single.request;
    expect(req.lastEventId, 'la-0');
    expect(req.after, 'la-0');
    expect(req.after, isNot(HfyClient.kZeroEventId),
        reason: 'a resumed boot whose pin was missed would query 00000000-… '
            'and replay the whole conversation');
  });

  test('item 8: racing connect() callers share ONE boot (identical futures, '
      'one history page, one stream)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final f1 = kit.client.connect();
    final f2 = kit.client.connect(); // the in-flight boot itself
    expect(identical(f1, f2), isTrue,
        reason: 'later callers are HANDED the in-flight future');
    await Future.wait<void>([f1, f2]);
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseOpenCount == 1, timeoutMs: 5000);
    final historyCalls = server.requests
        .where((r) => Uri.parse(r.path).path == '/v1/conversation/history')
        .length;
    expect(historyCalls, 1);
    final slots = server.requests
        .where((r) => Uri.parse(r.path).path == '/v1/conversation/uploads')
        .length;
    expect(slots, 0, reason: 'no stray uploads: exactly one boot');
  });

  test('item 8: pause()+resume() during an in-flight connect() defers to '
      'the boot — no second stream, the boot owns delivery', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-race');
    final boot = kit.client.connect();
    kit.client.pause();
    kit.client.resume(); // sees the in-flight boot and defers
    await boot;
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseOpenCount == 1, timeoutMs: 5000);
    final historyCalls = server.requests
        .where((r) => Uri.parse(r.path).path == '/v1/conversation/history')
        .length;
    expect(historyCalls, 1);
    expect(server.sseConnections.single.request.after, 'la-race');
  });

  test('item 8: a failed boot shares its error with racing callers and '
      'frees the slot for the next boot', () async {
    server.schedule401();
    server.schedule401();
    final f1 = kit.client.connect();
    final f2 = kit.client.connect();
    await expectLater(Future.wait<void>([f1, f2]),
        throwsA(isA<HfyAuthException>()));
    expect(kit.state, HfyConnectionState.error);
    expect(kit.tokens.callsIn('unauthorized'), 1,
        reason: 'one boot: one 401 refresh for both callers');
    // The slot is free again: a fresh boot works.
    server.historyPages[null] = historyPage(liveAfter: 'la-next');
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.state, HfyConnectionState.live);
  });

  test('item 9 (revised): pause() before the first connect() records an '
      'intent and moves no state; resume() clears it, so connect() boots',
      () async {
    expect(kit.state, HfyConnectionState.idle);
    kit.client.pause();
    expect(kit.state, HfyConnectionState.idle,
        reason: 'the intent waits for connect(); no state moves yet');
    kit.client.resume();
    expect(kit.state, HfyConnectionState.idle);
    server.historyPages[null] = historyPage(liveAfter: 'la-9');
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.state, HfyConnectionState.live,
        reason: 'the later boot works normally');
  });

  test('item 9: close() and dispose() release the client resources: events '
      'listeners get onDone, the state notifier disposes, an OWNED dio '
      'belongs to the client and dies with it', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    final done = Completer<void>();
    kit.client.events.listen(null, onDone: () => done.complete());
    await kit.client.dispose();
    await done.future.timeout(const Duration(seconds: 5));
    expect(done.isCompleted, isTrue, reason: 'the event controller is closed');
    // The state VALUE read is harmless after dispose (Flutter's notified
    // value getter does not assert); what a disposed notifier forbids is
    // any further state MOVE (close() would set `ended` again):
    expect(kit.client.state.value, HfyConnectionState.ended);
    // A dispose()d client must not be draggable back open (unlike close()):
    // the disposed notifier makes any further state move itself an error.
    expect(() => kit.client.close(), throwsA(anything));
  });
}
