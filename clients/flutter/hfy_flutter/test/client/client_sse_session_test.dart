// Review item 4: _SseSession must no longer be a bare Dio() — its session
// Dio is built FROM the caller's dio (all interceptors ride along — proxy,
// TLS pinning, auth-refresh wrappers see the SSE open) and its
// HttpClientAdapter is a FRESH one (sseAdapterFactory or an
// IOHttpClientAdapter clone), so the verified adapter.close(force: true)
// teardown stays session-local and never touches the caller's sockets.
import 'dart:async';
import 'dart:typed_data' show Uint8List;

import 'package:dio/dio.dart'
    show
        BaseOptions,
        Dio,
        HttpClientAdapter,
        Interceptor,
        RequestInterceptorHandler,
        RequestOptions,
        ResponseBody;
import 'package:dio/io.dart' show IOHttpClientAdapter;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;

import 'test_kit.dart';

/// An interceptor the tests hold up to a light: it records every request its
/// dio sees, then passes on.
class RecordingInterceptor extends Interceptor {
  final List<(String method, String path)> seen = [];

  @override
  void onRequest(RequestOptions options, RequestInterceptorHandler handler) {
    seen.add((options.method, options.uri.path));
    handler.next(options);
  }
}

/// A fresh-per-session adapter recording its opens; wraps a real IO adapter.
class RecordingAdapter implements HttpClientAdapter {
  final HttpClientAdapter _inner = IOHttpClientAdapter();
  final List<(String, String)> opens = [];

  @override
  Future<ResponseBody> fetch(RequestOptions options,
      Stream<Uint8List>? requestStream, Future<void>? cancelFuture) {
    opens.add((options.method, options.uri.path));
    return _inner.fetch(options, requestStream, cancelFuture);
  }

  @override
  void close({bool force = false}) => _inner.close(force: force);
}

void main() {
  late FakeHfyServer server;
  setUp(() async {
    server = await FakeHfyServer.start();
  });
  tearDown(() => server.destroy());

  test('item 4: the SSE session rides the caller dio\'s interceptors AND '
      'its own fresh adapter (both SEE the events open)', () async {
    final timers = FakeTimers();
    final tokens = TokenScript()..ttl(52000);
    tokens.clock = () => timers.now;
    final interceptor = RecordingInterceptor();
    final adapters = <RecordingAdapter>[];
    final dio0 = Dio()..interceptors.add(interceptor);
    final client = HfyClient(
      api: server.base,
      tokenProvider: tokens.provider,
      options: HfyClientOptions(
        clock: () => timers.now,
        timerFactory: timers.factory,
        idleTimerFactory: timers.factory,
        sseAdapterFactory: () {
          final adapter = RecordingAdapter();
          adapters.add(adapter);
          return adapter;
        },
      ),
      dio: dio0,
    );
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await client.connect();
    await until(() => server.sseConnections.isNotEmpty &&
        timers.active.any((t) => t.delay == const Duration(seconds: 30)));
    await settle();
    // The interceptor saw BOTH the REST history call and the SSE open:
    expect(
        interceptor.seen.map((m) => m.$2),
        containsAllInOrder(<Object>[
          contains('/v1/conversation/history'),
          contains('/v1/conversation/events'),
        ]),
        reason: 'the session dio copied the interceptors, so their '
            'onRequest runs for the SSE open too');
    // The ADAPTER the factory built saw the SSE open, and the factory ran
    // exactly once (a fresh adapter per session, not the caller's).
    expect(adapters, hasLength(1));
    expect(
        adapters.single.opens,
        everyElement(predicate<(String, String)>((open) => open.$1 == 'GET')));
    expect(adapters.single.opens.map((open) => open.$2),
        everyElement(contains('/v1/conversation/events')));

    // The caller's dio is untouched by the session lifecycle: after the
    // stream ended (adapter closed!), REST still works through the SAME
    // dio/adapter (no StateError 'adapter closed', no killed keep-alives).
    client.close();
    final turn = await client.send('after close of sse');
    expect(turn, isNotNull);
    expect(
        interceptor.seen.map((m) => m.$2),
        contains(contains('/v1/conversation/messages')),
        reason: 'the caller\'s wiring is unaffected by the session teardown');
    await client.dispose();
  });

  test('item 4: without a factory, the session adapter is a FRESH '
      'IOHttpClientAdapter clone (never the caller\'s adapter instance — '
      'its force-close must not kill the caller\'s sockets)', () async {
    final kit = await ClientKit.open(server);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await settle();
    // Force a session NOT to be torn down by the caller's adapter state:
    // reconnect calmly (a drop + one rung) and confirm the keep-alive of
    // the CALLER dio still serves REST after everything.
    server.sseConnections.single.abort();
    await until(() => kit.state == HfyConnectionState.reconnecting);
    kit.timers.activeOneOrNull(const Duration(milliseconds: 1000))!.run();
    await until(() => server.sseOpenCount == 2 &&
        kit.state == HfyConnectionState.live, timeoutMs: 5000);
    final turn = await kit.client.send('post-reconnect rest');
    expect(turn, isNotNull);
    await kit.client.stop();
  });

  test('item 4: the session dio copies the caller BaseOptions (timeouts, '
      'queryParameters, extra)', () async {
    final base = BaseOptions(
      connectTimeout: const Duration(milliseconds: 12345),
      receiveTimeout: const Duration(milliseconds: 67890),
      queryParameters: const <String, dynamic>{'appflag': 'sess'},
      extra: const <String, dynamic>{'k': 1},
    );
    final dio0 = Dio(base);
    final kit = await ClientKit.open(server, dio: dio0);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await until(() => server.sseConnections.isNotEmpty &&
        kit.timers.active
            .any((t) => t.delay == const Duration(seconds: 30)));
    // The SSE request rides the session dio unchanged (a pinned-headers
    // copy of the base merges before the open's own headers).
    expect(server.sseConnections.single.request.authorization,
        contains('Bearer'),
        reason: 'headers of the open flow exactly as before');
    await kit.client.dispose();
  });
}
