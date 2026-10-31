// Shared test kit for the HfyClient suite: token scripts, the paired
// client/server wiring and common envelope/history fixtures.
import 'package:dio/dio.dart' show Dio;
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyProtocolException;

import '../support/fake_hfy_server.dart';

export '../support/fake_hfy_server.dart';

/// mints increasing tokens with an expiry `ttlMs` after the fake clock when
/// the queue runs dry — expressed as an RFC3339 string, so the tolerated
/// expiry parse never trips the epoch-seconds branch (v < 1e12).
class TokenScript {
  final List<Object?> queued = [];
  final List<String> reasons = [];
  int _serial = 0;
  int _ttlMs = 52000;

  /// The fake clock the mints read (wired from options in makeClient).
  int Function()? clock;

  /// When set, calls past the script throw (default: mints forever).
  bool throwWhenEmpty = false;

  Future<Object?> provider(String reason) async {
    reasons.add(reason);
    if (queued.isNotEmpty) {
      final next = queued.removeAt(0);
      if (next == null) {
        throw const HfyProtocolException('onTokenNeeded returned no token');
      }
      return next;
    }
    if (throwWhenEmpty) {
      throw StateError('token script exhausted (reason: $reason)');
    }
    return <String, Object?>{
      'token': 'tok-${++_serial}',
      'expires_at': DateTime.fromMillisecondsSinceEpoch(
              (clock?.call() ?? 0) + _ttlMs,
              isUtc: true)
          .toIso8601String(),
    };
  }

  void queueGrant(String token, {required int expiresAtMs}) =>
      queued.add(<String, Object?>{
        'token': token,
        'expires_at': expiresAtMs, // epoch-ms branch of parseExpiryMs
      });

  void queueString(String token) => queued.add(token);

  void queueMap(Map<String, Object?> map) => queued.add(map);

  void queueNothing() => queued.add(null);

  void ttl(int ms) => _ttlMs = ms;

  int callsIn(String reason) => reasons.where((r) => r == reason).length;
}

/// History page builder (history.go's wire contract): turns oldest-first,
/// next/null, live_after, viewer.
Map<String, Object?> historyPage(
    {List<Object?>? turns,
    String? next,
    String? liveAfter,
    Map<String, Object?>? viewer}) {
  final page = <String, Object?>{
    'turns': turns ?? <Object?>[],
    'next': next,
    'live_after': liveAfter,
    'viewer': viewer ?? {'uuid': 'viewer-uuid', 'type': 'user', 'ref': 'alice'},
  };
  // The server always sends these keys (null allowed).
  return page;
}

/// Wire envelope: {id, type, parent?, actor, v, data?, created_at}.
Map<String, Object?> wireEnv(String id, String type,
        {String? parent, Object? data}) =>
    <String, Object?>{
      'id': id,
      'type': type,
      if (parent != null) 'parent': parent,
      'actor': {'uuid': 'act-1', 'type': 'agent', 'ref': 'main'},
      if (data != null) 'data': data,
      'created_at': '2026-01-01T00:00:00.000Z',
    };

/// One wired-up [HfyClient] against [server] with FakeTimers + TokenScript.
class ClientKit {
  final FakeHfyServer server;
  final FakeTimers timers;
  final TokenScript tokens;
  late final HfyClient client;

  ClientKit._(this.server, this.timers, this.tokens);

  /// [api] overrides the client's base (review item 2's prefix tests):
  /// `.../prefix` with [FakeHfyServer.pathPrefix] = '/prefix' set BEFORE
  /// this call. [dio] rides through to the client so SSE-session wiring
  /// (interceptors/adapter factory, review item 4) can be asserted.
  static Future<ClientKit> open(FakeHfyServer server,
      {int ttlMs = 52000, Uri? api, Dio? dio}) async {
    final timers = FakeTimers()..now = 0;
    final tokens = TokenScript()..ttl(ttlMs);
    tokens.clock = () => timers.now;
    final kit = ClientKit._(server, timers, tokens);
    kit.client = HfyClient(
      api: api ?? server.base,
      tokenProvider: tokens.provider,
      options: HfyClientOptions(
        clock: () => timers.now,
        timerFactory: timers.factory,
        idleTimerFactory: timers.factory,
      ),
      dio: dio,
    );
    return kit;
  }

  HfyConnectionState get state => client.state.value;

  /// Waits until the background SSE request completed: the idle watchdog
  /// arms exactly when the stream's headers are consumed (the connect-time
  /// feed of P-NAT-002). Deterministic, no fixed sleeps.
  Future<void> waitLive() => until(() =>
      server.sseConnections.isNotEmpty &&
      timers.active.any((t) => t.delay == const Duration(seconds: 30)));
}
