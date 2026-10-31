// The pause/resume state machine of HfyClient (the table on the class doc):
//   - a pause() BEFORE the first connect() is an intent: connect() loads
//     history into `paused` and opens no stream; the next resume() opens
//     exactly one;
//   - resume() before connect() clears the intent, so connect() streams;
//   - connect() never un-pauses: a connect() made while paused stays paused;
//   - a paused boot that fails stays paused, and resume() redoes it;
//   - two resume() calls around one pause() share one token fetch and start
//     one stream open;
//   - rapid pause/resume flapping never leaves two streams open.
// The fake server cannot see a force-closed client socket (see
// client_token_test.dart), and a redundant open may never reach it at all. So
// the open count comes from the client's own sseAdapterFactory hook, which
// runs once per open before its request goes out, and the live stream is
// proven by a frame written to the newest accepted connection.
import 'dart:async' show Completer;
import 'dart:math' show Random;

import 'package:dio/io.dart' show IOHttpClientAdapter;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart'
    show HfyClient, HfyClientOptions;
import 'package:hfy_flutter/src/client/connection.dart' show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart' show HfyHttpException;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  int historyCalls() => server.requests
      .where((r) => Uri.parse(r.path).path.endsWith('/history'))
      .length;

  /// A second client on [server]: its reconnect token fetch parks on [gate],
  /// and [onOpen] runs once per SSE open the client starts.
  HfyClient gatedClient(Completer<void> gate, void Function() onOpen) {
    final timers = FakeTimers();
    return HfyClient(
      api: server.base,
      tokenProvider: (reason) async {
        if (reason == 'unauthorized') await gate.future;
        return 'tok-$reason';
      },
      options: HfyClientOptions(
        clock: () => 0,
        timerFactory: timers.factory,
        idleTimerFactory: timers.factory,
        sseAdapterFactory: () {
          onOpen();
          return IOHttpClientAdapter();
        },
      ),
    );
  }

  /// Writes one frame to the newest accepted connection (the live stream) and
  /// returns the ids the client delivered. Superseded sockets are never
  /// written: a force-closed socket never completes its flush.
  Future<List<String>> deliveredFromNewest(List<String> ids) async {
    ids.clear();
    final newest = server.sseConnections.length - 1;
    await server.sseConnections[newest].addFrame(
      wireEnv('f$newest', 'message.text'),
    );
    await settle(40);
    return List.of(ids);
  }

  test('pause() before connect(): connect() loads history into paused and '
      'opens no stream; resume() opens exactly one', () async {
    kit.client.pause();
    expect(
      kit.state,
      HfyConnectionState.idle,
      reason: 'the intent moves no state',
    );
    server.historyPages[null] = historyPage(liveAfter: 'la-p');
    await kit.client.connect();
    expect(kit.state, HfyConnectionState.paused);
    expect(historyCalls(), 1, reason: 'the boot loaded its history page');
    expect(
      kit.client.lastEventId,
      'la-p',
      reason: 'live_after was pinned before the paused check',
    );
    expect(kit.client.viewer, isNotNull);
    expect(kit.timers.hasActiveTimers, isFalse);
    await settle(40);
    expect(server.sseOpenCount, 0, reason: 'zero SSE opens while paused');

    kit.client.resume();
    await until(
      () => kit.state == HfyConnectionState.live && server.sseOpenCount == 1,
      timeoutMs: 5000,
    );
    await settle(40);
    expect(server.sseOpenCount, 1, reason: 'resume() opened exactly one');
    final req = server.sseConnections.single.request;
    expect(req.after, 'la-p');
    expect(req.lastEventId, 'la-p');
  });

  test(
    'resume() before connect() clears the intent: connect() streams normally',
    () async {
      kit.client.pause();
      kit.client.resume();
      server.historyPages[null] = historyPage(liveAfter: 'la-r');
      await kit.client.connect();
      await kit.waitLive();
      expect(kit.state, HfyConnectionState.live);
      expect(server.sseOpenCount, 1);
    },
  );

  test('connect() while paused loads history and stays paused: no un-pause, no '
      'stream', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    kit.client.pause();
    await kit.client.connect();
    expect(kit.state, HfyConnectionState.paused);
    expect(historyCalls(), 2, reason: 'the paused connect() reloaded history');
    await settle(40);
    expect(server.sseOpenCount, 1, reason: 'no second stream while paused');

    kit.client.resume();
    await until(
      () => kit.state == HfyConnectionState.live && server.sseOpenCount == 2,
      timeoutMs: 5000,
    );
  });

  test(
    'a paused boot that FAILS stays paused (no error state); resume() redoes '
    'the boot and opens one stream',
    () async {
      kit.client.pause();
      server.scriptedErrors.add(('/v1/conversation/history', 503, 'down'));
      server.historyPages[null] = historyPage(liveAfter: 'la-f');
      await expectLater(kit.client.connect(), throwsA(isA<HfyHttpException>()));
      expect(
        kit.state,
        HfyConnectionState.paused,
        reason: 'the failure arrived while paused: no error state',
      );
      expect(server.sseOpenCount, 0);

      kit.client.resume();
      await until(
        () => kit.state == HfyConnectionState.live && server.sseOpenCount == 1,
        timeoutMs: 5000,
      );
      expect(historyCalls(), 2, reason: 'resume() redid the failed boot');
      expect(server.sseConnections.single.request.after, 'la-f');
    },
  );

  test('two resume() calls around one pause() share one token fetch and start '
      'one stream open', () async {
    final gate = Completer<void>();
    var attempts = 0;
    final client = gatedClient(gate, () => attempts++);
    server.historyPages[null] = historyPage(liveAfter: 'la-g');
    await client.connect();
    await until(
      () =>
          client.state.value == HfyConnectionState.live &&
          server.sseOpenCount == 1,
      timeoutMs: 5000,
    );
    expect(attempts, 1, reason: 'the boot started one open');

    client.pause();
    client.resume(); // chain #1 parks on the gated token fetch
    client.pause();
    client.resume(); // chain #2 shares the same fetch
    gate.complete();
    await until(
      () =>
          client.state.value == HfyConnectionState.live &&
          server.sseOpenCount == 2,
      timeoutMs: 5000,
    );
    await settle(40);
    expect(
      attempts,
      2,
      reason: 'the second chain started no open: one resume, one stream',
    );

    final ids = <String>[];
    client.events.listen((event) => ids.add(event.id!));
    expect(await deliveredFromNewest(ids), [
      'f1',
    ], reason: 'only the live stream delivers');
  });

  test(
    'a pause() while the reconnect token fetch is parked opens no stream',
    () async {
      final gate = Completer<void>();
      final gated = await ClientKit.open(
        server,
        provider: (reason) async {
          if (reason == 'unauthorized') await gate.future;
          return 'tok-$reason';
        },
      );
      server.historyPages[null] = historyPage(liveAfter: 'la-w');
      await gated.client.connect();
      await gated.waitLive();

      gated.client.pause();
      gated.client.resume(); // the reconnect parks on the token fetch
      gated.client.pause(); // pause lands before the fetch answers
      gate.complete();
      await settle(40);
      expect(gated.state, HfyConnectionState.paused);
      expect(server.sseOpenCount, 1, reason: 'the parked chain must not open');
    },
  );

  test('rapid pause/resume flapping never leaves two streams open', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-x');
    var attempts = 0;
    final client = gatedClient(Completer<void>()..complete(), () => attempts++);
    await client.connect();
    await until(
      () =>
          client.state.value == HfyConnectionState.live &&
          server.sseOpenCount == 1,
      timeoutMs: 5000,
    );
    final ids = <String>[];
    client.events.listen((event) => ids.add(event.id!));

    final random = Random(7);
    var resumes = 0;
    for (var i = 0; i < 40; i++) {
      client.pause();
      if (random.nextBool()) {
        await Future<void>.delayed(Duration.zero);
      }
      client.resume();
      resumes++;
      if (random.nextInt(3) == 0) {
        await settle(2);
      }
    }
    // The last call is a resume: the client must end live, one stream.
    await until(
      () => client.state.value == HfyConnectionState.live,
      timeoutMs: 5000,
    );
    await settle(60);
    expect(
      attempts,
      lessThanOrEqualTo(1 + resumes),
      reason: 'each resume starts at most one open',
    );
    expect(
      server.sseOpenCount,
      lessThanOrEqualTo(1 + resumes),
      reason: 'the fake server saw no more connections than opens',
    );
    final delivered = await deliveredFromNewest(ids);
    expect(
      delivered,
      hasLength(1),
      reason: 'one frame per round: exactly one stream is still consumed',
    );
  });
}
