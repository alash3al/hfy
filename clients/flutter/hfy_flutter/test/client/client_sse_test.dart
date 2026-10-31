// SSE behavior: open wiring (P-CLIENT-026/033), no-gap/no-dup resumption
// across a forced drop, dedupe of replayed envelopes, ping-only keep-alive
// and idle death (P-NAT-002), stream end at grant expiry, unparseable frames
// (P-CLIENT-027), parser oversize => reconnect, a 401'ed open (P-CLIENT-030)
// and single-flight connect() racing (review item 8). All driven against the
// in-process FakeHfyServer; event-driven or bounded waits only (§11-2).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;

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

  test('P-CLIENT-026/033: the SSE open carries Bearer in the Authorization '
      'header (never a token query), Last-Event-ID and ?after=',
      () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-open');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    expect(server.sseOpenCount, 1);
    final req = conn().request;
    expect(req.authorization, 'Bearer tok-1');
    expect(req.lastEventId, 'la-open');
    expect(req.after, 'la-open');
    expect(req.path.contains('token='), isFalse,
        reason: 'the token never rides the URL (dio can set headers)');
  });

  test('P-CLIENT-033 + dedupe: resume after a mid-stream drop has NO gaps '
      'and NO duplicates; the resume rides Last-Event-ID', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await kit.client.connect();
    await kit.waitLive();
    await settle();

    await conn().addFrame(wireEnv('e1', 'turn'));
    await conn().addFrame(wireEnv('e2', 'message.text'));
    await conn().addFrame(wireEnv('e3', 'message.text'));
    await until(() => kit.client.lastEventId == 'e3');

    conn().abort(); // grant revoked / dropped mid-stream
    await until(() => kit.state == HfyConnectionState.reconnecting);
    final ladder = kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!;
    ladder.run(); // the first rung of client.js's ladder
    await until(() =>
        server.sseConnections.length == 2 &&
        kit.state == HfyConnectionState.live);

    // Reopened from e3: the replayed e3 must be deduped, e4 delivered.
    final conn2 = server.sseConnections.last;
    expect(conn2.request.lastEventId, 'e3');
    expect(conn2.request.after, 'e3');
    expect(conn2.request.authorization, 'Bearer tok-2',
        reason: 'reconnect mints a fresh token first (P-NAT-002)');
    await conn2.addFrame(wireEnv('e3', 'message.text')); // exact replay
    await until(() => ids.length == 3);
    expect(ids, ['e1', 'e2', 'e3'],
        reason: 'a replayed envelope is absorbed before any listener');
    await conn2.addFrame(wireEnv('e4', 'message.text'));
    await until(() => ids.length == 4);
    expect(ids, ['e1', 'e2', 'e3', 'e4'], reason: 'no gap after the replay');
    expect(kit.client.lastEventId, 'e4');
    expect(kit.state, HfyConnectionState.live);
  });

  test('P-CLIENT-015: a replayed live envelope is delivered exactly once',
      () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    final frame = wireEnv('dup-1', 'message.text');
    await conn().addFrame(frame);
    await until(() => ids.length == 1);
    await conn().addFrame(frame); // exact same envelope again
    await settle(50);
    expect(ids, ['dup-1'], reason: 'the second delivery is absorbed whole');
  });

  test('P-NAT-002: ping-only silence keeps the stream alive; true silence '
      'dies and reconnects from the last id', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    expect(kit.timers.activeWithDelay(const Duration(seconds: 30)), hasLength(1));

    // A ping is a byte: the watchdog re-arms WITHOUT dying.
    final armedBeforePing =
        kit.timers.activeWithDelay(const Duration(seconds: 30)).single;
    await conn().addPing();
    await until(() => armedBeforePing.cancelled);
    expect(armedBeforePing.cancelled, isTrue, reason: 'fed, re-armed');
    expect(kit.state, HfyConnectionState.live);
    expect(server.sseOpenCount, 1, reason: 'no reconnect on pings');

    // Fire the (dead) pre-ping handle: nothing happens — it was cancelled.
    armedBeforePing.run();
    await settle();
    expect(kit.state, HfyConnectionState.live);

    // Now TRUE silence: firing the CURRENT armed window kills the stream.
    kit.timers.activeWithDelay(const Duration(seconds: 30)).single.run();
    await settle();
    expect(kit.state, HfyConnectionState.reconnecting,
        reason: '30s of silence = dead; the session adapter force-closed it '
            '(dart:io keeps the zombie invisible to the server side)');
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseConnections.length == 2);
    expect(conn().request.after, 'la-0', reason: 'resume from the pin');
  });

  test('P-NAT-002: stream end (grant expiry) -> re-mint -> reconnect from '
      'lastEventId', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    await conn().addFrame(wireEnv('before-end', 'message.text'));
    await until(() => kit.client.lastEventId == 'before-end');
    server.sseConnections.single.closeClean(); // server ended the stream
    await until(() => kit.state == HfyConnectionState.reconnecting);
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await until(() => server.sseConnections.length == 2 &&
        kit.state == HfyConnectionState.live);
    expect(conn().request.lastEventId, 'before-end');
    expect(conn().request.authorization, 'Bearer tok-2');
  });

  test('P-CLIENT-027: unparseable frame data drops silently, no reconnect',
      () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    await conn().write('data: not json at all;\n\n');
    final frame = wireEnv('real-1', 'message.text');
    await conn().addFrame(frame);
    await until(() => ids.length == 1);
    expect(ids, ['real-1']);
    expect(kit.state, HfyConnectionState.live);
  });

  test('P-NAT-002 oversize framing: a parser HfyProtocolException routes the '
      'stream to the reconnect ladder instead of crashing', () async {
    // This mirrors the SSE fixer wave's added parser size limits: a line
    // far beyond any sane cap must throw HfyProtocolException out of
    // HfySseParser.push and be recovered via reconnect.
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    final giant =
        'id: huge\nevent: message.text\ndata: ${'A' * (32 * 1024 * 1024)}\n\n';
    await conn().write(giant);
    await until(() => kit.state == HfyConnectionState.reconnecting,
        timeoutMs: 8000);
    expect(ids, isEmpty, reason: 'no oversize envelope ever fired');
    expect(kit.state, HfyConnectionState.reconnecting,
        reason: 'the protocol break surfaced as a reconnect, never a crash');
  });

  test('P-CLIENT-030 analog: a 401 on the SSE open stays on the ladder, then '
      'a fresh token reopens', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await until(() => kit.state == HfyConnectionState.live);
    // The NEXT events open (the reconnect one) answers 401 once.
    server.schedule401();
    conn().abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await settle(30); // let the doomed open reach the server
    expect(kit.state, HfyConnectionState.reconnecting);
    // The rejected open is an API request (recorded), not an SSE connection.
    expect(
        server.requests.where((r) => Uri.parse(r.path).path ==
                '/v1/conversation/events').length,
        2,
        reason: 'the 401 attempt was a real events open, then the retry');
    expect(
        kit.timers.activeOneOrNull(const Duration(milliseconds: 2000)),
        isNotNull,
        reason: 'a failed open did NOT reset the attempts');
    kit.timers.activeOneOrNull(const Duration(milliseconds: 2000))!.run();
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseConnections.length == 2,
        timeoutMs: 8000);
    expect(conn().request.authorization, 'Bearer tok-3',
        reason: 'tok-1 initial; one ladder run minted tok-2, then tok-3');
  });

  test('P-CLIENT-008 + review item 8: racing connect() callers share ONE '
      'boot — one history page, one SSE open, identical futures', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    final f1 = kit.client.connect();
    final f2 = kit.client.connect(); // the in-flight boot, not a second one
    await Future.wait<void>([f1, f2]);
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseOpenCount == 1);
    expect(kit.state, HfyConnectionState.live);
    expect(kit.tokens.callsIn('initial'), 1,
        reason: 'one boot: one initial fetch (single-flight throughout)');
    final historyCalls = server.requests
        .where((r) => Uri.parse(r.path).path == '/v1/conversation/history')
        .length;
    expect(historyCalls, 1,
        reason: 'review item 8: ONE in-flight connect future, later callers '
            'await it instead of double-loading and double-opening');
    // A single stream owns delivery:
    await conn().addFrame(wireEnv('alive-1', 'message.text'));
    await until(() => ids.length == 1);
    expect(ids, ['alive-1']);
  });

  test('P-CLIENT-004: a connect() AFTER a completed boot is the client.js '
      'no-op (not a second boot, not a new stream)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    final opensBefore = server.sseOpenCount;
    await kit.client.connect();
    await settle(30);
    expect(server.sseOpenCount, opensBefore);
    expect(kit.state, HfyConnectionState.live);
  });
}
