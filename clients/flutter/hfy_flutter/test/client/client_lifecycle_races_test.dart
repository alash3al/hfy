// Lifecycle-defect fixes, verified against the fake server:
//   - close()/dispose() racing an in-flight connect() boot must never leave
//     a client that ends `live` but never delivers: the generation counter
//     abandons every await step, clears the single-flight slot and closes
//     any session opened mid-boot (BLOCKER fix);
//   - dispose() from inside an events listener (re-entrancy) runs its whole
//     teardown instead of dying in the sync broadcast controller's
//     "Cannot fire new event" — and a second dispose is a no-op;
//   - resume() after a boot killed by a failure redoes the FULL history
//     boot (connect semantics), never an all-zero-id SSE reopen.
// All interleavings are deterministic: a boot parks at its first await, so
// close()/dispose() issued afterwards run mid-boot without wall-clock; for
// mid-history races the fake server holds the answer via holdRequests.
import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart'
    show HfyHttpException, HfyNetworkException;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  int historyCalls({required FakeHfyServer server}) => server.requests
      .where((r) => Uri.parse(r.path).path.endsWith('/history'))
      .length;

  test('BLOCKER: connect(); close(); ends `ended`, opens no SSE, fetches no '
      'history and leaves no timers; connect() completes silently',
      () async {
    final boot = kit.client.connect(); // parked at the token fetch
    kit.client.close();
    await expectLater(boot, completes,
        reason: 'the abandoned boot must not surface its own failure '
            '(the state was already moved to ended)');
    await settle(30);
    expect(kit.state, HfyConnectionState.ended);
    expect(server.requests, isEmpty,
        reason: 'the boot abandoned BEFORE the history HTTP call');
    expect(server.sseOpenCount, 0);
    expect(kit.timers.hasActiveTimers, isFalse,
        reason: 'nothing armed timers for the dead boot');
  });

  test('BLOCKER: connect(); close(); connect() returns a FRESH boot — not '
      'the stale flight — and ends live', () async {
    final f1 = kit.client.connect();
    kit.client.close();
    final f2 = kit.client.connect();
    expect(identical(f1, f2), isFalse,
        reason: 'close() cleared the single-flight slot; the second connect '
            'must not be handed the dead flight');
    await expectLater(f1, completes); // the abandoned boot, silently
    server.historyPages[null] = historyPage(liveAfter: 'la-fresh');
    await expectLater(f2, completes);
    await kit.waitLive();
    expect(kit.state, HfyConnectionState.live);
    expect(server.sseOpenCount, 1,
        reason: 'only the fresh boot opened the stream');
    expect(historyCalls(server: server), 1,
        reason: 'the abandoned boot never loaded history');
  });

  test('BLOCKER: connect(); close(); connect(); close(); connect() ends '
      'live exactly once', () async {
    final f1 = kit.client.connect();
    kit.client.close();
    final f2 = kit.client.connect();
    kit.client.close();
    final f3 = kit.client.connect();
    await expectLater(f1, completes);
    await expectLater(f2, completes);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await expectLater(f3, completes);
    await kit.waitLive();
    await until(() =>
        kit.state == HfyConnectionState.live && server.sseOpenCount == 1);
    expect(historyCalls(server: server), 1);
    expect(kit.timers.hasActiveTimers, isTrue,
        reason: 'the live client owns its idle watchdog and refresh timer');
  });

  test('BLOCKER: dispose() during the boot (token fetch parked) runs its '
      'whole teardown: no state move, no socket, no timer, events closed',
      () async {
    final gate = Completer<void>();
    final parked = await ClientKit.open(
      server,
      provider: (reason) async {
        await gate.future;
        return 'tok-parked';
      },
    );
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final boot = parked.client.connect(); // parked at the token fetch
    final eventsDone = Completer<void>();
    parked.client.events.listen(null, onDone: () => eventsDone.complete());
    await parked.client.dispose();
    await eventsDone.future.timeout(const Duration(seconds: 5));
    expect(parked.timers.hasActiveTimers, isFalse);
    expect(parked.client.state.value, HfyConnectionState.ended);
    expect(server.requests, isEmpty,
        reason: 'the boot was parked client-side; nothing reached the wire');
    expect(server.sseOpenCount, 0);
    gate.complete(); // release: the zombie boot wakes into a dead client
    await expectLater(boot, completes,
        reason: 'the abandoned boot must not surface a StateError');
    await settle(30);
    expect(parked.timers.hasActiveTimers, isFalse,
        reason: 'no timer armed after the client was disposed');
    expect(server.sseOpenCount, 0);
    expect(server.requests, isEmpty);
  });

  test('BLOCKER: dispose() during the boot (history answer held) starts no '
      'session, arms no timer and completes without a StateError', () async {
    final gate = Completer<void>();
    server.holdRequests = gate;
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final boot = kit.client.connect();
    final eventsDone = Completer<void>();
    kit.client.events.listen(null, onDone: () => eventsDone.complete());
    await until(() =>
        server.requests.any((r) => r.path.contains('/v1/conversation/history')));
    await kit.client.dispose();
    await eventsDone.future.timeout(const Duration(seconds: 5));
    expect(kit.timers.hasActiveTimers, isFalse);
    expect(kit.client.state.value, HfyConnectionState.ended);
    gate.complete(); // the history answers into a dead client
    await expectLater(boot, completes,
        reason: 'the abandoned boot swallows the failure of the dead client');
    await settle(30);
    expect(server.sseOpenCount, 0,
        reason: 'the orphaned boot abandoned before _openStream()');
    expect(kit.timers.hasActiveTimers, isFalse);
  });

  test('BLOCKER: close() during the token fetch leaves zero wire traffic',
      () async {
    final gate = Completer<void>();
    final parked = await ClientKit.open(
      server,
      provider: (reason) async {
        await gate.future;
        return 'tok-parked';
      },
    );
    final boot = parked.client.connect();
    parked.client.close();
    expect(parked.state, HfyConnectionState.ended);
    gate.complete();
    await expectLater(boot, completes);
    await settle(20);
    expect(server.requests, isEmpty);
    expect(server.sseOpenCount, 0);
    expect(parked.timers.hasActiveTimers, isFalse);
  });

  test('BLOCKER: close() during the history load abandons the boot: no SSE '
      'open, no timers, ended', () async {
    final gate = Completer<void>();
    server.holdRequests = gate;
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final boot = kit.client.connect();
    await until(() =>
        server.requests.any((r) => r.path.contains('/v1/conversation/history')));
    kit.client.close();
    expect(kit.state, HfyConnectionState.ended);
    gate.complete();
    await expectLater(boot, completes);
    await settle(30);
    expect(server.sseOpenCount, 0);
    expect(kit.timers.hasActiveTimers, isFalse);
  });

  test('BLOCKER: close() during the ladder wait cancels everything', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    server.sseConnections.single.abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    kit.client.close(); // during the ladder WAIT (a rung is pending)
    await settle(20);
    expect(kit.state, HfyConnectionState.ended);
    expect(kit.timers.hasActiveTimers, isFalse,
        reason: 'the rung was cancelled, nothing re-armed');
    expect(server.sseOpenCount, 1);
  });

  test('BLOCKER: a rung fired right before close() must not open a second '
      'SSE from the orphaned chain (generation checked after its fetch)',
      () async {
    final gate = Completer<void>();
    final parked = await ClientKit.open(
      server,
      provider: (reason) async {
        if (reason == 'unauthorized') await gate.future; // the rung parks here
        return 'tok-rung';
      },
    );
    server.historyPages[null] = historyPage(liveAfter: 'la-r');
    await parked.client.connect();
    await parked.waitLive();
    server.sseConnections.single.abort();
    await until(() => parked.state == HfyConnectionState.reconnecting);
    parked.timers
        .activeOneOrNull(const Duration(milliseconds: 1000))!
        .run(); // _runReconnect(): fresh token fetch, parked
    parked.client.close(); // mid-fetch: the chain is orphaned
    expect(parked.state, HfyConnectionState.ended);
    gate.complete();
    await settle(30);
    expect(parked.timers.hasActiveTimers, isFalse,
        reason: 'the orphaned ladder chain checked its generation and '
            'stopped before _openStream()');
    expect(server.sseOpenCount, 1,
        reason: 'the zombie chain never reopened the stream');
    expect(parked.client.state.value, HfyConnectionState.ended);
  });

  test('BLOCKER: resume() after close() is a no-op (no sneaky reopen while '
      'a boot is being abandoned)', () async {
    final boot = kit.client.connect();
    kit.client.close();
    kit.client.resume();
    await settle(30);
    expect(kit.state, HfyConnectionState.ended);
    expect(server.sseOpenCount, 0);
    await expectLater(boot, completes);
  });

  test('item 5: pause() during a boot that then FAILS makes resume() redo '
      'the FULL history boot — SSE from the fresh live_after pin, viewer '
      'captured, no all-zero reopen', () async {
    final gate = Completer<void>();
    server.holdRequests = gate;
    server.scriptedErrors.add(('/v1/conversation/history', 503, 'no thanks'));
    server.historyPages[null] = historyPage(liveAfter: 'la-fresh');
    final boot = kit.client.connect();
    await until(() =>
        server.requests.any((r) => r.path.contains('/v1/conversation/history')));
    kit.client.pause(); // pause WHILE the history answer is withheld
    gate.complete(); // the history answers 503: the boot fails mid-boot
    await expectLater(boot, throwsA(isA<HfyHttpException>()));
    expect(kit.state, HfyConnectionState.paused,
        reason: 'the failure arrived while paused: no error state — but the '
            'history flag stayed false, which is exactly the defect case');
    expect(server.sseOpenCount, 0);

    kit.client.resume();
    await until(
        () => kit.state == HfyConnectionState.live && server.sseOpenCount == 1,
        timeoutMs: 5000);
    expect(historyCalls(server: server), 2,
        reason: 'resume() redid the history boot instead of opening SSE '
            'directly');
    final req = server.sseConnections.single.request;
    expect(req.after, 'la-fresh',
        reason: 'the redo pinned live_after from the redone history page — '
            'not the all-zero id');
    expect(req.lastEventId, 'la-fresh');
    expect(kit.client.viewer, isNotNull,
        reason: 'the redo captured the viewer (own-ness computable)');
    expect(kit.state, HfyConnectionState.live);
  });

  test('item 2: dispose() from inside an events listener runs the whole '
      'teardown: onDone still delivered, state ended, the owned dio dead, '
      'and a second dispose() is a silent no-op', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    final got = <String>[];
    Future<void>? disposeFuture;
    final eventsDone = Completer<void>();
    runZonedGuarded(() {
      kit.client.events.listen((event) {
        got.add(event.id!);
        disposeFuture ??= kit.client.dispose(); // RE-ENTRANT dispose
      });
      kit.client.events.listen(null, onDone: () => eventsDone.complete());
    }, (Object error, StackTrace stackTrace) {
      fail('dispose() threw out of the listener: $error');
    });
    await server.sseConnections.single.addFrame(wireEnv('kill-1', 'turn'));
    await until(() => got.isNotEmpty && disposeFuture != null,
        timeoutMs: 5000);
    await disposeFuture!.timeout(const Duration(seconds: 5));
    expect(eventsDone.isCompleted, isTrue,
        reason: 'the deferred controller close still delivers onDone');
    expect(kit.client.state.value, HfyConnectionState.ended);
    expect(kit.timers.hasActiveTimers, isFalse);
    disposeFuture = null;
    await expectLater(kit.client.dispose(), completes,
        reason: 'a second dispose() is an idempotent no-op');
    // The owned Dio was closed: any REST ask dies with the adapter's
    // closed-adapter StateError (the resource release, observable).
    await expectLater(kit.client.send('x'), throwsA(isA<HfyNetworkException>()));
  }, timeout: const Timeout(Duration(seconds: 15)));

  test('item 2: every dispose() call after the first is a no-op', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await expectLater(kit.client.dispose(), completes);
    await expectLater(kit.client.dispose(), completes);
    await expectLater(kit.client.dispose(), completes);
    expect(kit.client.state.value, HfyConnectionState.ended);
    await expectLater(kit.client.send('x'), throwsA(isA<HfyNetworkException>()));
  });
}
