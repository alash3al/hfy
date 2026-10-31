// REST timeouts (fix: dio.fetch(RequestOptions(...)) never merges BaseOptions):
//   - the caller dio's connect/receive/send timeouts ride every REST request
//     itself, with documented 30s/60s defaults when the app dio sets none;
//   - a black-holed endpoint (accept, never answer) fails send() with the
//     typed HfyNetworkException inside the configured window, not forever;
//   - the SSE open stays timeout-FREE: dio applies receiveTimeout between
//     stream chunks, so the idle watchdog owns that stream's liveness.
// The spy interceptor observes the per-request options (an interceptor on the
// caller dio is copied onto every SSE session dio), so no wall-clock is
// needed for the option plumbing; the only timing assert is the small
// receiveTimeout on the held server.
import 'dart:async';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/errors.dart'
    show HfyNetworkException;

import 'test_kit.dart';

class TimeoutSpy extends Interceptor {
  /// REST requests (method, connect, receive, send).
  final List<(String, Duration?, Duration?, Duration?)> rest = [];

  /// Every STREAM response type request (the SSE opens).
  final List<(Duration?, Duration?)> stream = [];

  @override
  void onRequest(RequestOptions options, RequestInterceptorHandler handler) {
    if (options.responseType == ResponseType.stream) {
      stream.add((options.connectTimeout, options.receiveTimeout));
    } else {
      rest.add((options.method, options.connectTimeout, options.receiveTimeout,
          options.sendTimeout));
    }
    handler.next(options);
  }
}

void main() {
  late FakeHfyServer server;
  setUp(() async {
    server = await FakeHfyServer.start();
  });
  tearDown(() => server.destroy());

  test('the caller dio\'s timeouts ride the REST requests; a fast server '
      'answers inside them', () async {
    final spy = TimeoutSpy();
    final dio0 = Dio(BaseOptions(
      connectTimeout: const Duration(milliseconds: 90),
      receiveTimeout: const Duration(milliseconds: 80),
      sendTimeout: const Duration(milliseconds: 70),
    ))..interceptors.add(spy);
    final kit = await ClientKit.open(server, dio: dio0);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    final turn = await kit.client.send('timeouts on the wire');
    expect(turn, isNotNull, reason: 'the small-but-honest timeouts subsisted');
    final post = spy.rest.where((r) => r.$1 == 'POST').single;
    expect(post.$2, const Duration(milliseconds: 90), reason: 'connectTimeout');
    expect(post.$3, const Duration(milliseconds: 80), reason: 'receiveTimeout');
    expect(post.$4, const Duration(milliseconds: 70), reason: 'sendTimeout');
    await kit.client.dispose();
  });

  test('the SSE open carries NO timeouts (receiveTimeout would clamp the '
      'stream\'s chunk gaps; the idle watchdog owns liveness)', () async {
    final spy = TimeoutSpy();
    final dio0 = Dio(BaseOptions(
      connectTimeout: const Duration(milliseconds: 90),
      receiveTimeout: const Duration(milliseconds: 80),
      sendTimeout: const Duration(milliseconds: 70),
    ))..interceptors.add(spy);
    final kit = await ClientKit.open(server, dio: dio0);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    expect(spy.stream, hasLength(1));
    expect(spy.stream.single.$1, isNull, reason: 'no connectTimeout on SSE');
    expect(spy.stream.single.$2, isNull,
        reason: 'no receiveTimeout on SSE: dio applies it BETWEEN chunks and '
            'the watchdog is the liveness authority');
    await kit.client.dispose();
  });

  test('defaults when the app dio sets none: documented 30s connect / 60s '
      'receive / send unset (no wall-clock — the fast server answers)',
      () async {
    final spy = TimeoutSpy();
    final dio0 = Dio()..interceptors.add(spy);
    final kit = await ClientKit.open(server, dio: dio0);
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await kit.client.send('record the inline options');
    final post = spy.rest.where((r) => r.$1 == 'POST').single;
    expect(post.$2, const Duration(seconds: 30), reason: 'connect default');
    expect(post.$3, const Duration(seconds: 60), reason: 'receive default');
    expect(post.$4, isNull, reason: 'sendTimeout copied as-is (unset)');
    await kit.client.dispose();
  });

  test('a never-answering endpoint fails send() with HfyNetworkException '
      'inside the configured timeout (no hang)', () async {
    server.holdRequests = Completer<void>(); // black hole: never released
    final dio0 = Dio(BaseOptions(
      receiveTimeout: const Duration(milliseconds: 120),
    ));
    final kit = await ClientKit.open(server, dio: dio0);
    final sw = Stopwatch()..start();
    await expectLater(kit.client.send('to a black hole'),
        throwsA(isA<HfyNetworkException>()));
    sw.stop();
    expect(sw.elapsed, lessThan(const Duration(seconds: 3)),
        reason: 'the 120ms receiveTimeout fired, not a hang nor the 30s '
            'connect default');
  });
}
