// An in-process fake of the hfy conversation API (docs/reference.md "API",
// wire contract internal/httpapi/*.go) for the HfyClient tests: dart:io
// HttpServer only, no extra packages. Grant endpoints don't exist; the tests
// mint tokens by hand and script responses per test.
//
// What it supports (phase task test list):
//   - request RECORDS (method, path, body bytes, authorization,
//     Last-Event-ID, content-type) for byte-exact payload checks;
//   - scripted history PAGES keyed by the exact `before` query value
//     (null key = the no-before first call); a missing key answers 404;
//   - scripted SSE: frames (`id/event/data`), `: ping` comments, a clean
//     close (the grant-expiry end) and an abrupt abort (mid-stream drop),
//     with open/closed socket accounting for the no-leak check;
//   - 401 answers (all requests or the next N) for the
//     401->refresh->retry-once and second-401 rules;
//   - both /v1/conversation/config shapes;
//   - the upload slot plus both PUT flavors (direct absolute, hfy path
//     WITH the grant), recorded body-exact.
import 'dart:async';
import 'dart:convert' show jsonEncode, utf8;
import 'dart:io';

import 'package:hfy_flutter/src/client/connection.dart'
    show HfyTimerFactory;

/// One recorded request: headers lower-cased, body the raw text.
class RecordedRequest {
  final String method;
  final String path;
  final Map<String, String> headers;
  final String body;

  RecordedRequest(
      {required this.method,
      required this.path,
      required this.headers,
      required this.body});

  String get authorization => headers['authorization'] ?? '';

  String? get lastEventId => headers['last-event-id'];

  String get after => Uri.parse(path).queryParameters['after'] ?? '';

  String get contentType => headers['content-type'] ?? '';
}

/// One open SSE connection the test controls (write frames/pings, then
/// closeClean() for a grant-expiry end or abort() for a mid-stream drop).
class FakeSseConnection {
  final RecordedRequest request;
  final HttpResponse response;
  bool closed = false;

  FakeSseConnection(this.request, this.response);

  Future<void> write(String data) async {
    if (closed) return;
    try {
      response.write(data);
      await response.flush();
    } catch (e) {
      closed = true; // the peer is gone; the test never breaks on this
    }
  }

  Future<void> addFrame(Map<String, Object?> envelope) async {
    await write('id: ${envelope['id']}\n'
        'event: ${envelope['type']}\n'
        'data: ${jsonEncode(envelope)}\n\n');
  }

  Future<void> addPing() async => write(': ping\n\n');

  Future<void> closeClean() async {
    if (closed) return;
    closed = true;
    await response.close();
  }

  /// Ends the stream mid-flight (a drop / revoked grant). dart:io has no
  /// response.destroy(); by the time a test aborts, headers and at least the
  /// opening comment were written, so detachSocket would throw — a close()
  /// the CLIENT treats the same: end-of-stream => the ladder (P-NAT-002).
  void abort() {
    if (closed) return;
    closed = true;
    response.close();
  }
}

class FakeHfyServer {
  FakeHfyServer._();

  late HttpServer _server;

  String get host => '127.0.0.1';
  int get port => _server.port;
  Uri get base => Uri.parse('http://$host:$port');

  // -- scripted behavior (all mutable fields are per-test) ------------------

  /// Scheduled 401 answers, consumed FIFO: one entry per upcoming request
  /// that must answer 401 {"error": "..."} instead of proceeding.
  final List<void> _pending401 = <void>[];

  void schedule401() => _pending401.add(null);

  /// Scripted non-401 error answers, consumed FIFO: `(exact path, status,
  /// error text)` — used to map every status of reference.md to the typed
  /// exceptions. Served as `{"error": <text>}`.
  final List<(String, int, String)> scriptedErrors = [];

  /// History pages keyed by the exact `before` query value (null key = the
  /// first call). A missing key answers 404 {"error": "not found"}.
  final Map<String?, Map<String, Object?>> historyPages = {};

  /// POST /v1/conversation/messages -> this turn id in `{"turn": ...}`.
  String turnForMessages = 'turn-fixed-1';

  /// config(): null (no filesystem) or the {types: [...], max_bytes: N} map,
  /// served under "uploads" verbatim, so both shapes reach the client exact.
  Object? configUploads;

  /// POST /v1/conversation/uploads -> this slot map verbatim
  /// ({file, upload_url, expires_at, direct}).
  Map<String, Object?> uploadSlot = {};

  // -- records --------------------------------------------------------------

  final List<RecordedRequest> requests = [];

  final List<String> messagesBodies = [];
  final List<String> approvalsBodies = [];
  final List<RecordedRequest> uploadPuts = [];
  final List<String> uploadPutBodies = [];
  final List<FakeSseConnection> sseConnections = [];

  int get sseOpenCount => sseConnections.length;
  int get sseClosedCount =>
      sseConnections.where((c) => c.closed).length;

  RecordedRequest requestWhere(bool Function(RecordedRequest) test) =>
      requests.singleWhere(test);
  RecordedRequest? requestWhereOrNull(bool Function(RecordedRequest) test) =>
      requests.where(test).firstOrNull;

  /// Longest wait for every SSE socket to see closure. Real hfy discovers a
  /// dead peer through its `: ping` write (a write into a destroyed socket
  /// throws); this fake does the same — it pings open connections while
  /// polling, so a client-cancelled socket surfaces as closed here.
  Future<void> awaitSseAllClosed() async {
    for (var i = 0; i < 100; i++) {
      if (sseClosedCount >= sseOpenCount) return;
      final open = sseConnections.where((c) => !c.closed).toList();
      if (open.isEmpty) return;
      for (final c in open) {
        await c.addPing(); // errors land in write(); closed flags itself
      }
      await Future<void>.delayed(const Duration(milliseconds: 10));
    }
    throw StateError('fake server: still-open SSE sockets after 1s');
  }

  // -- lifecycle ------------------------------------------------------------

  static Future<FakeHfyServer> start() async {
    final s = FakeHfyServer._()
      .._server = await HttpServer.bind('127.0.0.1', 0);
    s._server.listen(s._onRequest, onError: (Object _) {});
    return s;
  }

  Future<void> destroy() async {
    for (final c in List.of(sseConnections)) {
      c.abort();
    }
    await _server.close(force: true);
  }

  // -- routing --------------------------------------------------------------

  Future<void> _onRequest(HttpRequest raw) async {
    final res = raw.response;
    final headers = <String, String>{};
    raw.headers.forEach((name, values) => headers[name.toLowerCase()] = values.join(', '));
    final record = RecordedRequest(
        method: raw.method,
        path: raw.uri.toString(),
        headers: headers,
        body: await utf8.decoder.bind(raw).join());
    requests.add(record);

    res.headers.set('Content-Type', 'application/json');
    if (_pending401.isNotEmpty) {
      _pending401.removeAt(0);
      await _json(res, 401, {'error': 'invalid or expired token'});
      return;
    }
    final scripted = scriptedErrors
        .indexWhere((spec) => spec.$1 == raw.uri.path);
    if (scripted >= 0) {
      final spec = scriptedErrors.removeAt(scripted);
      await _json(res, spec.$2, {'error': spec.$3});
      return;
    }

    switch (raw.uri.path) {
      case '/v1/conversation/history':
        final before = raw.uri.queryParameters['before'];
        final key = before == null || before.isEmpty ? null : before;
        if (!historyPages.containsKey(key)) {
          await _json(res, 404, {'error': 'not found'});
          return;
        }
        await _json(res, 200, historyPages[key]!);
        return;
      case '/v1/conversation/messages':
        messagesBodies.add(record.body);
        await _json(res, 202, {'turn': turnForMessages});
        return;
      case '/v1/conversation/approvals':
        approvalsBodies.add(record.body);
        await _json(res, 202, {});
        return;
      case '/v1/conversation/stop':
        await _json(res, 202, {});
        return;
      case '/v1/conversation/config':
        await _json(res, 200, {'uploads': configUploads});
        return;
      case '/v1/conversation/uploads':
        await _json(res, 201, uploadSlot);
        return;
      case '/v1/conversation/events':
        if (raw.method != 'GET') {
          await _json(res, 405, {'error': 'method not allowed'});
          return;
        }
        final conn = FakeSseConnection(record, res);
        sseConnections.add(conn);
        unawaited(res.done.then((_) {
          conn.closed = true;
        }, onError: (Object _) {
          conn.closed = true;
        }));
        res.bufferOutput = false; // stream like Go's http.Flusher does
        res.headers.set('Content-Type', 'text/event-stream');
        res.headers.set('Cache-Control', 'no-cache');
        await res.flush();
        // dart:io quirk: headers only hit the wire with the FIRST body byte
        // (flush() alone does not send them, unlike Go's http.Flusher). A
        // comment line is protocol-invisible to the parser and gives dio its
        // headers immediately — the client then starts consuming bytes.
        res.write(': go\n\n');
        await res.flush();
        return;
      case '/v1/files/serve-direct':
        uploadPuts.add(record); // "presigned": recorded without its auth check
        uploadPutBodies.add(record.body);
        await _json(res, 200, {});
        return;
      default:
        if (raw.uri.path.startsWith('/v1/conversation/uploads/') &&
            raw.method == 'PUT') {
          uploadPuts.add(record); // the slot's hfy-path upload_url
          uploadPutBodies.add(record.body);
          await _json(res, 204, {});
          return;
        }
        await _json(res, 404, {'error': 'not found'});
        return;
    }
  }

  Future<void> _json(HttpResponse res, int code, Object? body) async {
    res.statusCode = code;
    res.write(_encode(body));
    await res.close();
  }

  static String _encode(Object? body) => body == null ? '' : jsonEncode(body);
}

// -- test harness: scriptable timers (PARITY.md §11-2: no real waits) ------

/// One injected timer handle: `(delay, callback)` recorded, never scheduled.
class FakeTimer implements Timer {
  final Duration delay;
  final void Function() callback;

  bool fired = false;
  bool cancelled = false;

  FakeTimer(this.delay, this.callback);

  @override
  void cancel() => cancelled = true;

  @override
  bool get isActive => !fired && !cancelled;

  @override
  int get tick => 0;

  void run() {
    if (!isActive) return;
    fired = true;
    callback();
  }
}

/// The injectable clock + timer factory the client tests drive by hand:
/// every timer the client or the idle watchdog schedules is recorded with
/// its DELAY (the backoff-ladder and refresh-skew assertions read this list)
/// and can be fired explicitly. A close()d client must leave this list
/// empty of active handles (the no-leaked-timers check).
class FakeTimers {
  final List<FakeTimer> all = [];

  /// The wall clock (epoch ms). Tests advance it explicitly.
  int now = 0;

  /// The HfyClientOptions.timerFactory / idleTimerFactory wiring.
  FakeTimer factory(Duration delay, void Function() callback) {
    final t = FakeTimer(delay, callback);
    all.add(t);
    return t;
  }

  HfyTimerFactory get timerFactory => factory;

  List<FakeTimer> get active => all.where((t) => t.isActive).toList();

  /// All currently-active timers with an exact delay.
  List<FakeTimer> activeWithDelay(Duration delay) =>
      active.where((t) => t.delay == delay).toList();

  FakeTimer? activeOneOrNull(Duration delay) {
    final hits = activeWithDelay(delay).toList(growable: false);
    return hits.isEmpty ? null : hits.single;
  }

  bool get hasActiveTimers => active.isNotEmpty;
}

// -- test harness: microtask/event-loop settling for real-socket IO --------

/// Lets pending HTTP/microtask work settle (small bounded sleeps so the fake
/// server's socket writes land between steps).
Future<void> settle([int ms = 10]) =>
    Future<void>.delayed(Duration(milliseconds: ms));

/// Waits until [cond] holds (polling; deterministic instead of fixed sleeps).
Future<void> until(bool Function() cond, {int timeoutMs = 2000}) async {
  final deadline = DateTime.now().add(Duration(milliseconds: timeoutMs));
  while (!cond()) {
    if (DateTime.now().isAfter(deadline)) {
      throw StateError('until: condition never held within ${timeoutMs}ms');
    }
    await Future<void>.delayed(const Duration(milliseconds: 2));
  }
}

/// Envelope factory matching the wire shape (id/type/parent/actor/v/data).
Map<String, Object?> env(String id, String type,
        {String? parent, Object? data, String actor = 'agent-main'}) =>
    {
      'id': id,
      'type': type,
      if (parent != null) 'parent': parent,
      'actor': {'uuid': 'u-$actor', 'type': 'agent', 'ref': 'main'},
      'v': 1,
      'data': data ?? {},
      'created_at': '2026-01-01T00:00:00Z',
    };

