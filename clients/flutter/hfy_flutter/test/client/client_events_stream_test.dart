// The events stream itself (review items 1+3) and the malformed-envelope
// contract: an ORDINARY Dart broadcast stream — `.first`/`firstWhere`/
// `await for`/pause/resume/unsubscribing all behave like the platform
// promises — with per-listener exception isolation, plus a live
// HfyEvent.fromJson-rejected envelope that drops WITHOUT the reconnect
// ladder, claims its id in the cursor, and surfaces via onProtocolError.
import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/model/event.dart'
    show HfyEvent, HfyProtocolException;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  FakeSseConnection conn() => server.sseConnections.last;

  Future<void> boot() async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
  }

  test('review item 1: `.first` resolves on the first accepted envelope',
      () async {
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't1', 'events': [wireEnv('h1', 'turn')]},
        ],
        liveAfter: 'la-1');
    final first = kit.client.events.first; // BEFORE connect: still waits
    await kit.client.connect();
    expect((await first).id, 'h1', reason: 'an ordinary stream: .first works');
  });

  test('review item 1: `firstWhere` predicate works like every stream',
      () async {
    await boot();
    final hit = kit.client.events
        .firstWhere((event) => event.id == 'w2')
        .timeout(const Duration(seconds: 5));
    await conn().addFrame(wireEnv('w1', 'turn'));
    await conn().addFrame(wireEnv('w2', 'message.text'));
    expect((await hit).id, 'w2', reason: 'the search predicate need not be '
        'the FIRST envelope — a real stream scans');
  });

  test('review item 1: `await for` receives TWO back-to-back frames in '
      'order (no crash, no loss)', () async {
    await boot();
    final got = <String>[];
    final done = () async {
      await for (final event in kit.client.events) {
        got.add(event.id!);
        if (got.length >= 2) break;
      }
    }();
    await conn().addFrame(wireEnv('b1', 'turn'));
    await conn().addFrame(wireEnv('b2', 'turn'));
    await done.timeout(const Duration(seconds: 5));
    expect(got, ['b1', 'b2']);
  });

  test('review item 1: pause() buffers, resume() delivers the missed frames',
      () async {
    await boot();
    final got = <String>[];
    late final StreamSubscription<HfyEvent> sub;
    sub = kit.client.events.listen((event) => got.add(event.id!));
    await conn().addFrame(wireEnv('p1', 'turn'));
    await until(() => got.isNotEmpty);
    sub.pause();
    await conn().addFrame(wireEnv('p2', 'turn'));
    await conn().addFrame(wireEnv('p3', 'turn'));
    await settle(30);
    expect(got, ['p1'], reason: 'paused subscription must not see frames yet');
    sub.resume();
    await until(() => got.length == 3, timeoutMs: 5000);
    expect(got, ['p1', 'p2', 'p3'],
        reason: 'a back-to-back pair buffered while paused arrives on '
            'resume, in order');
  });

  test('review item 1: a THROWING listener never aborts delivery — same '
      'dispatch reaches the others, later frames still arrive, the client '
      'stays live, and the error reaches that listener\'s zone uncaught '
      'handler', () async {
    await boot();
    final otherGot = <String>[];
    final zoneErrors = <Object>[];
    // The stream reports a listener's own exception through THAT listener's
    // zone (dart's runUnaryGuarded rules, documented on `events`).
    runZonedGuarded(() {
      kit.client.events.listen((event) {
        throw StateError('boom on ${event.id}');
      });
      kit.client.events.listen((event) => otherGot.add(event.id!));
    }, (error, stackTrace) {
      zoneErrors.add(error);
    });
    await conn().addFrame(wireEnv('e1', 'turn'));
    await until(() => otherGot.isNotEmpty, timeoutMs: 5000);
    expect(otherGot, ['e1'],
        reason: 'the other listener received the frame in the SAME dispatch');
    // Later frames of the SAME stream still arrive:
    await conn().addFrame(wireEnv('e2', 'turn'));
    await until(() => otherGot.length == 2, timeoutMs: 5000);
    expect(otherGot, ['e1', 'e2']);
    expect(zoneErrors, isNotEmpty,
        reason: 'the listener exception surfaced through the zone');
    // Nothing about the connection changed: no reconnect ladder, state live.
    expect(kit.state, HfyConnectionState.live);
    expect(server.sseOpenCount, 1);
    expect(kit.timers.reconnectRungsActive, isFalse,
        reason: 'no backoff rungs armed by a listener error');
  });

  test('review item 1: a listener cancelling ITSELF mid-dispatch is safe; '
      'other listeners continue', () async {
    await boot();
    final runnerGot = <String>[];
    final seen = <String>[];
    late final StreamSubscription<HfyEvent> leaver;
    runZonedGuarded(() {
      leaver = kit.client.events.listen((event) {
        seen.add(event.id!);
        leaver.cancel(); // cancel DURING the sync dispatch
      });
    }, (_, __) {});
    kit.client.events.listen((event) => runnerGot.add(event.id!));
    await conn().addFrame(wireEnv('c1', 'turn'));
    await until(() => runnerGot.isNotEmpty, timeoutMs: 5000);
    expect(seen, ['c1']);
    await conn().addFrame(wireEnv('c2', 'turn'));
    await until(() => runnerGot.length == 2, timeoutMs: 5000);
    expect(runnerGot, ['c1', 'c2'], reason: 'unrelated listeners unaffected');
  });

  test('review item 3: one malformed live envelope is DROPPED without '
      'reconnecting; its id is claimed so the cursor moves past it; later '
      'frames still arrive; the rejection surfaces via onProtocolError',
      () async {
    final rejections = <HfyProtocolException>[];
    kit.client.onProtocolError = rejections.add;
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await boot();

    await conn()
        .addFrame(<String, Object?>{ // actor NOT an object => fromJson rejects
          'id': 'bad-1',
          'type': 'message.text',
          'actor': 'i am just a string',
          'v': 1,
          'data': {'text': 'hi'},
          'created_at': '2026-01-01T00:00:00Z',
        });
    await until(() => rejections.isNotEmpty, timeoutMs: 5000);
    expect(rejections.single.message, 'actor must be an object');
    expect(ids, isEmpty, reason: 'the dead envelope never reaches listeners');

    // NO reconnect: one open, live, no ladder rungs.
    expect(server.sseOpenCount, 1);
    expect(kit.state, HfyConnectionState.live);
    expect(kit.timers.reconnectRungsActive, isFalse);

    // The cursor already moved past the bad envelope: the replay on a
    // reconnect would be absorbed; and a later LIVE frame is delivered.
    await conn().addFrame(wireEnv('good-2', 'turn'));
    await until(() => ids.length == 1, timeoutMs: 5000);
    expect(ids, ['good-2']);
    expect(kit.client.lastEventId, 'good-2',
        reason: 'claim(bad-1) advanced the cursor off the zero/ashed pin');

    // The bad id is IN the seen-set: an exact replay is absorbed silently.
    await conn().addFrame(wireEnv('bad-1', 'message.text'));
    await settle(30);
    expect(ids, ['good-2'],
        reason: 'the replayed malformed id never delivers twice');

    // …and a resume() would query Last-Event-ID=good-2, NOT the zero uuid —
    // here: a stream drop then the reconnect carrying the cursor.
    conn().abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await until(() => server.sseOpenCount == 2, timeoutMs: 5000);
    expect(conn().request.lastEventId, 'good-2');
  });

  test('review item 3: a HISTORY envelope rejected by the model is dropped '
      'and claimed in the seen-set, never reconnecting', () async {
    final rejections = <HfyProtocolException>[];
    kit.client.onProtocolError = rejections.add;
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't1', 'events': [
            <String, Object?>{'id': 'h-bad', 'type': 'turn', 'actor': 'nope'},
            wireEnv('h-good', 'turn'),
          ]},
        ],
        liveAfter: 'la-1');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await kit.client.connect();
    await kit.waitLive();
    await until(() => ids.length == 1, timeoutMs: 5000);
    expect(ids, ['h-good']);
    expect(rejections.single.message, 'actor must be an object');
    expect(kit.client.lastEventId, 'la-1',
        reason: 'history deliveries never advance the cursor');
    expect(kit.state, HfyConnectionState.live);
    expect(server.sseOpenCount, 1);
  });

  test('review item 3: a v-is-not-int rejection (numeric data variant) does '
      'the same drop/claim/surface, no reconnect', () async {
    final rejections = <HfyProtocolException>[];
    kit.client.onProtocolError = rejections.add;
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await boot();
    await conn()
        .addFrame(<String, Object?>{
      'id': 'bad-v',
      'type': 'message.text',
      'v': 'two', // v must be an int
      'actor': {'uuid': 'u1', 'type': 'agent', 'ref': 'main'},
      'data': {'text': 'x'},
      'created_at': '2026-01-01T00:00:00Z',
    });
    await until(() => rejections.isNotEmpty, timeoutMs: 5000);
    expect(rejections.single.message, 'event envelope v must be an int');
    await conn().addFrame(wireEnv('good-v', 'turn'));
    await until(() => ids.length == 1, timeoutMs: 5000);
    expect(ids, ['good-v']);
    expect(kit.client.lastEventId, 'good-v');
  });
}

extension on FakeTimers {
  bool get reconnectRungsActive {
    const rungs = <Duration>[
      Duration(milliseconds: 1000),
      Duration(milliseconds: 2000),
      Duration(milliseconds: 5000),
    ];
    return active.any((t) => rungs.contains(t.delay));
  }
}
