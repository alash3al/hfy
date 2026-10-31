// Typed error surface the review items require (items 5/10/11): a transport
// failure surfaces as HfyNetworkException whose toString carries no token
// (the raw DioException's requestOptions embed the bearer), a 3xx is NOT
// followed (followRedirects off on ALL hfy requests — REST and SSE) and
// becomes a typed HfyHttpException, and server-supplied error text is
// stripped of control characters and clamped to 512 chars (a proxy's HTML
// error page cannot become a giant message).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart';

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  setUp(() async {
    server = await FakeHfyServer.start();
  });
  tearDown(() => server.destroy());

  test('item 5: a failed connect() throws HfyNetworkException (not a raw '
      'DioException) whose toString carries NO token', () async {
    // An unbound loopback port: connection refused in microseconds.
    final client = HfyClient(
      api: Uri.parse('http://127.0.0.1:1'),
      tokenProvider: (_) async =>
          <String, Object?>{'token': 'tok-leaky-secret', 'expires_at': 60000},
    );
    Object? caught;
    try {
      await client.connect();
      fail('connect to a dead host must fail');
    } on HfyNetworkException catch (e) {
      caught = e;
    }
    expect(caught, isNotNull);
    expect('$caught'.contains('tok-leaky-secret'), isFalse,
        reason: 'HfyNetworkException carries type + short text only; the '
            'raw DioException embeds requestOptions.headers');
    expect('$caught'.toLowerCase().contains('bearer'), isFalse);
    await client.dispose();
  });

  test('item 5: a send() over a DEAD SOCKET also surfaces as '
      'HfyNetworkException without the token in the message', () async {
    // Grab the fake server's port, then DESTROY it so nothing listens
    // anymore: the next request hits a closed socket.
    final port = server.port;
    await server.destroy();
    final client = HfyClient(
      api: Uri.parse('http://127.0.0.1:$port'),
      tokenProvider: (_) async =>
          <String, Object?>{'token': 'tok-dead-socket', 'expires_at': 60000},
    );
    await expectLater(
        client.send('hi'),
        throwsA(isA<HfyNetworkException>().having(
            (e) => '$e',
            'toString()',
            predicate<String>((s) => !s.contains('tok-dead-socket')))));
    await client.dispose();
  });

  test('item 10: the server error text is stripped of control characters '
      'and clamped at 512 chars', () async {
    final kit = await ClientKit.open(server);
    final giant =
        '<html>\u0007proxy页面\r\n${'x' * 600}</html>';
    server.scriptedErrors.add(('/v1/conversation/stop', 404, giant));
    try {
      await kit.client.stop();
      fail('stop() must fail on the scripted 404');
    } on HfyHttpException catch (e) {
      expect(e.message.length, 512,
          reason: 'well past the cap, so the clamp bit in');
      expect(e.message.contains('\n'), isFalse);
      expect(e.message.contains('\r'), isFalse);
      expect(e.message.contains('\u0007'), isFalse);
      expect(e.message.contains('\u0000'), isFalse);
      // The original text rode along intact in the diagnostic body only.
      final body = e.body is Map ? Map<String, Object?>.from(e.body as Map) : null;
      expect(body?['error'], isNotNull);
    }
  });

  test('item 11: a 3xx is NOT followed anywhere on REST — the typed error '
      'is the answer, and NO second request happened', () async {
    final kit = await ClientKit.open(server);
    server.redirects.add(
        ('/v1/conversation/stop', 307, '${server.base}/v1/steal-token'));
    await expectLater(kit.client.stop(),
        throwsA(isA<HfyHttpException>()
            .having((e) => e.status, 'status', 307)
            .having((e) => e.message, 'message', 'HTTP 307')));
    final stops = server.requests
        .where((r) => server.pathPrefix.isEmpty
            ? r.path.startsWith('/v1')
            : true)
        .where((r) =>
            Uri.parse(r.path).path.endsWith('/v1/conversation/stop') ||
            Uri.parse(r.path).path.endsWith('/v1/steal-token'))
        .toList();
    expect(stops, hasLength(1),
        reason: 'dio must NOT follow: one request, no redirect hop');
  });

  test('item 11: an SSE open answering 3xx routes to the ladder as a '
      'non-2xx open (no token hop, no lost stream)', () async {
    final kit = await ClientKit.open(server);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    // The NEXT events open answers a redirect instead of opening a stream.
    server.redirects.add(('/v1/conversation/events', 302,
        '${server.base}/v1/steal-token'));
    server.sseConnections.single.abort();
    await until(() => kit.state == HfyConnectionState.reconnecting,
        timeoutMs: 5000);
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await until(() => kit.state == HfyConnectionState.reconnecting &&
        server.requests.where((r) =>
            Uri.parse(r.path).path.endsWith('/v1/conversation/events') ||
            Uri.parse(r.path).path.endsWith('/v1/steal-token')).length >= 2,
        timeoutMs: 5000);
    await settle(30);
    final eventsOpens = server.requests
        .where((r) =>
            Uri.parse(r.path).path.endsWith('/v1/conversation/events'))
        .length;
    expect(eventsOpens, 2,
        reason: 'the live open and the 302-d open (the second did NOT '
            'establish a stream: openCount stays 1)');
    expect(server.sseOpenCount, 1,
        reason: 'a redirect never opened an SSE stream');
    final stole = server.requests
        .where((r) => Uri.parse(r.path).path.endsWith('/v1/steal-token'))
        .length;
    expect(stole, 0, reason: 'dio must NOT follow: no token ever hops');
    expect(kit.state, HfyConnectionState.reconnecting,
        reason: 'a 3xx open is a dead open: back on the ladder');
  });
}
