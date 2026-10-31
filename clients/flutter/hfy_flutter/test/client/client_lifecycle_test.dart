// Lifecycle: close() stops everything with no leaked timers or sockets
// (P-CLIENT-014), events never fire after close (P-CLIENT-014/016 + 034's
// nuance), a connect() after close() revives (P-CLIENT-034), pause()/resume()
// (P-NAT-004 analog) resuming from lastEventId, and the backoff ladder of
// P-CLIENT-030. All timings injectable (PARITY.md §11-2: no real waits).
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

  /// The single active LADDER timer (one of the [1000, 2000, 5000] rungs).
  /// The idle watchdog (30s) and the refresh timer (26s for the 52s mint)
  /// never share these rungs.
  FakeTimer ladder() {
    const rungs = <Duration>[
      Duration(milliseconds: 1000),
      Duration(milliseconds: 2000),
      Duration(milliseconds: 5000),
    ];
    final active = kit.timers.active
        .where((t) => rungs.contains(t.delay))
        .toList();
    return active.single;
  }

  test('P-CLIENT-014: close() closes the stream, cancels every timer and '
      'leaves zero live handles (no leaked timers, no sockets)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseConnections.length == 1);
    expect(kit.state, HfyConnectionState.live);
    // Boot leaves exactly two handles: the idle watchdog and the refresh
    // timer (with ttl 52s minted at t=0, the refresh delay is 26s… together
    // with the 30s idle handle = 2 active).
    await until(() => kit.timers.active.length >= 2);
    expect(kit.timers.active.length, 2,
        reason: 'the idle watchdog AND the refresh timer (ttl 52s mint at '
            't=0 => its delay is 26s) both live');

    kit.client.close();
    await settle(50);
    expect(kit.state, HfyConnectionState.ended);
    expect(kit.timers.hasActiveTimers, isFalse,
        reason: 'the refresh AND the idle watchdog were both cancelled');
    await settle(50);
    expect(kit.timers.hasActiveTimers, isFalse,
        reason: 'nothing re-arms itself after close');
    // NOTE: the socket-death side of this need not be observable here: dart:io
    // hides a client force-closed SSE socket from the server (its writes into
    // the zombie succeed silently and response.done never fires), so the
    // session adapter force-close is verified indirectly: a server frame then
    // still writes "successfully" but the client's subscription is gone
    // (the no-events test below).
  });

  test('P-CLIENT-014: no events fire after close() — even frames already '
      'sent from the server', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await conn().addFrame(wireEnv('pre-close', 'message.text'));
    await until(() => ids.length == 1);
    kit.client.close();
    await conn().addFrame(wireEnv('post-close', 'message.text'));
    await settle(50);
    expect(ids, ['pre-close'],
        reason: 'no post-close envelope can ever reach listeners');
    // (dart:io hides a client force-closed socket from the server, so its
    // closure is not observable here — see the note in the close() test.)
  });

  test('P-CLIENT-034: connect() after close() runs the full boot again',
      () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    kit.client.close();
    await settle(50);
    server.historyPages[null] = historyPage(liveAfter: 'la-1');
    await kit.client.connect();
    await kit.waitLive();
    await until(() => server.sseConnections.length == 2 &&
        kit.state == HfyConnectionState.live);
    expect(conn().request.after, 'la-1', reason: 'fresh boot, fresh pin');
    expect(server.sseOpenCount, 2);
  });

  test('P-NAT-004 analog: pause() closes the stream and cancels timers '
      'WITHOUT ending the client; resume() reconnects from lastEventId', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    await conn().addFrame(wireEnv('live-1', 'message.text'));
    await until(() => kit.client.lastEventId == 'live-1');

    kit.client.pause();
    await until(() => kit.state == HfyConnectionState.paused);
    await settle(50);
    expect(kit.timers.hasActiveTimers, isFalse,
        reason: 'refresh + idle watchdog cancelled; the client is NOT closed');
    expect(kit.client.lastEventId, 'live-1', reason: 'cursor kept for resume');
    expect(kit.client.viewer, isNotNull, reason: 'grant state kept alive');

    kit.client.pause(); // idempotent
    expect(kit.state, HfyConnectionState.paused);

    kit.client.resume();
    await until(() => server.sseConnections.length == 2 &&
        kit.state == HfyConnectionState.live);
    expect(conn().request.lastEventId, 'live-1',
        reason: 'resume reconnects FROM lastEventId, no gap');
    expect(conn().request.authorization, 'Bearer tok-2',
        reason: 'resume mints a fresh token (the ladder path, P-CLIENT-030)');

    kit.client.resume(); // idempotent: a stream is already open
    kit.client.close();
    await settle(50);
    expect(kit.state, HfyConnectionState.ended);
  });

  test('P-NAT-004 analog: resume() after close() does nothing (connect() is '
      'the revival, P-CLIENT-034)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    kit.client.pause();
    kit.client.close();
    final opensBefore = server.sseOpenCount;
    kit.client.resume();
    await settle(30);
    expect(server.sseOpenCount, opensBefore);
    expect(kit.state, HfyConnectionState.ended);
  });

  test('P-NAT-004 analog: resume() on a never-connected client waits for '
      'connect() (no stream, no token fetch)', () async {
    kit.client.pause();
    kit.client.resume();
    await settle(30);
    expect(server.sseOpenCount, 0);
    expect(kit.tokens.reasons, isEmpty);
  });

  test('P-CLIENT-030: the ladder is [1000, 2000, 5000, 5000…] until an open '
      'succeeds; a successful open resets to the first rung (P-CLIENT-029)',
      () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    expect(kit.state, HfyConnectionState.live);

    // Drop the stream: attempt 0.
    conn().abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    expect(ladder().delay, const Duration(milliseconds: 1000));
    server.schedule401(); // the upcoming events open answers 401
    ladder().run();
    await until(() =>
        kit.timers.activeOneOrNull(const Duration(milliseconds: 2000)) != null);
    expect(ladder().delay, const Duration(milliseconds: 2000),
        reason: 'a failed open leaves the attempt count growing');

    server.schedule401();
    ladder().run();
    await until(() =>
        kit.timers.activeOneOrNull(const Duration(milliseconds: 5000)) != null);
    expect(ladder().delay, const Duration(milliseconds: 5000));

    server.schedule401();
    ladder().run();
    await until(() =>
        kit.timers.activeOneOrNull(const Duration(milliseconds: 5000)) != null);
    expect(ladder().delay, const Duration(milliseconds: 5000),
        reason: 'the last rung repeats forever (capped)');

    // Now let an open succeed: live, and a later drop starts from rung 0.
    ladder().run();
    await until(() => kit.state == HfyConnectionState.live &&
        server.sseConnections.length == 2);
    conn().abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    expect(ladder().delay, const Duration(milliseconds: 1000),
        reason: 'the successful open reset the attempts (P-CLIENT-029)');
  });
}
