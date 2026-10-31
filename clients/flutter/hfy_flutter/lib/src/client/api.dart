// The dio wire layer: one port per endpoint of docs/reference.md "API", with
// the EXACT request contract of the embedded web client (client.js #request,
// #loadHistory, readBody — PARITY.md P-CLIENT-018..023):
//   - Bearer token on every request, `Content-Type: application/json` only
//     when a body exists, body = JSON string (client.js:203-211);
//   - 401 => exactly one token refresh (reason `unauthorized`) and one retry
//     with retries disabled, only while not close()d (client.js:215-219,
//     P-CLIENT-021);
//   - error message = the `{"error": ...}` text, else 'HTTP `<status>`' —
//     typed as HfyHttpException/HfyAuthException (client.js:220-226);
//   - body reading: text -> JSON.parse, parse failure -> `{error: text}`,
//     empty -> null treated by callers as `{}` (client.js:319-329);
//   - history `?limit=50` + `&before=<cursor>`; `next` becomes the cursor;
//     `viewer` is captured by the client BEFORE any event fires (P-CLIENT-018).
//
// The SSE request is a dio stream GET on /v1/conversation/events. The token
// travels ONLY in the Authorization header (dio can set headers, which
// dart-jvm cannot through EventSource — the web client uses `?token=` for
// exactly that reason; the header is the safer default here, `_queryToken`
// stays unused). Deviation is safe by wire contract: withGrant accepts
// bearer or ?token= (internal/httpapi/server.go:578-585).
//
// Tokens never appear in logs or URLs: only in the一览 header above.
import 'dart:async' show Future, unawaited;
import 'dart:convert' show jsonDecode, jsonEncode;

import 'package:dio/dio.dart'
    show Dio, RequestOptions, Response, ResponseBody, ResponseType;

import '../model/event.dart' show HfyActor, HfyProtocolException;
import 'connection.dart' show HfyClock;
import 'errors.dart' show HfyHttpException;
import 'token.dart' show HfyGrant;

int _systemClock() => DateTime.now().millisecondsSinceEpoch;

/// Token state of one client: validation, the live grant and single-flight
/// fetching (client.js #fetchToken at 175-181, P-CLIENT-017; concurrent
/// callers share one in-flight refresh — a native hardening the browser got
/// for free by serializing callbacks). The provider may answer a plain token
/// string, a `{token, expires_at?}` map or a [HfyGrant]; all are normalized
/// by HfyGrant.fromResult / direct passthrough.
class HfyTokenGate {
  final Future<Object?> Function(String reason) provider;
  final HfyClock clock;

  HfyGrant? current;
  Future<HfyGrant>? _inFlight;

  HfyTokenGate(this.provider, {HfyClock? clock})
      : clock = clock ?? _systemClock;

  /// The live grant's expiry in epoch ms; null when unknown (no proactive
  /// refresh then — client.js #expiresAtMs, P-CLIENT-024).
  int? get expiresAtMs => current?.expiresAtMs;

  String? get token => current?.token;

  /// Fetches a grant for [reason]; concurrent calls share one provider call
  /// and its result (single-flight). The gate rejects a Missing/empty token
  /// with the same message client.js throws ('onTokenNeeded returned no
  /// token', client.js:178).
  Future<HfyGrant> fetch(String reason) {
    final inFlight = _inFlight;
    if (inFlight != null) return inFlight; // single-flight
    final session = () async {
      final result = await provider(reason);
      if (result is HfyGrant) {
        if (result.token.isEmpty) {
          throw const HfyProtocolException('onTokenNeeded returned no token');
        }
        current = result;
        return result;
      }
      // Every other shape (plain string / {token, expires_at}) goes through
      // the tolerant parser, which throws the same error on no/empty token.
      final grant = HfyGrant.fromResult(result);
      current = grant;
      return grant;
    }();
    _inFlight = session;
    void clear() {
      if (identical(_inFlight, session)) _inFlight = null;
    }

    // Side chain that clears the single-flight slot; its errors are already
    // delivered to every fetch() caller above.
    final tokenFuture = session;
    unawaited(tokenFuture.then((_) {
      clear();
    }, onError: (Object e, StackTrace st) {
      clear();
    }));
    return session;
  }
}

/// One opened SSE response: the status headers plus the byte stream.
class HfyEventResponse {
  final int status;
  final ResponseBody body;

  const HfyEventResponse(this.status, this.body);
}

/// One endpoint per hfy method; all return decoded JSON maps (`{}` for an
/// empty body — the callers-shape rule of P-CLIENT-023).
class HfyApi {
  final Dio dio;
  final HfyTokenGate gate;

  /// The api base, trailing slashes stripped (client.js #api, P-CLIENT-004).
  final Uri base;

  /// Mirrors client.js #closed while the drive is closed: no initial token
  /// fetch and no 401 retry while true (P-CLIENT-014). The HfyClient owns it.
  bool closed = false;

  /// client.js calls #scheduleRefresh() right after an unauthorized refresh
  /// (client.js:217); the client wires its own timer logic here.
  void Function()? onUnauthorizedRefresh;

  HfyApi({
    required this.dio,
    required this.base,
    required this.gate,
  }) {
    final scheme = base.scheme;
    if (scheme != 'http' && scheme != 'https') {
      throw ArgumentError.value(base, 'api', 'must be an http(s) URL');
    }
  }

  // -- REST -----------------------------------------------------------------

  /// GET /v1/conversation/history?limit=50[&before=<cursor>] (P-CLIENT-018).
  Future<Map<String, Object?>> history({String? before}) {
    var path = '/v1/conversation/history?limit=50';
    if (before != null) {
      path += '&before=${Uri.encodeComponent(before)}';
    }
    return _request(path);
  }

  /// POST /v1/conversation/messages `{text}` + `attachments` only when
  /// defined (client.js:114-118, P-CLIENT-010).
  Future<Map<String, Object?>> sendMessage(
      String text, List<Object?>? attachments) {
    final body = <String, Object?>{'text': text};
    if (attachments != null) body['attachments'] = attachments;
    return _request('/v1/conversation/messages', method: 'POST', body: body);
  }

  /// POST /v1/conversation/uploads `{mime, size}` -> the upload slot
  /// (client.js:121, uploads.go requestUpload: 201 {file, upload_url,
  /// expires_at, direct}).
  Future<Map<String, Object?>> requestUploadSlot(String mime, int size) =>
      _request('/v1/conversation/uploads',
          method: 'POST', body: <String, Object?>{'mime': mime, 'size': size});

  /// PUT one upload slot. `direct` (presigned S3): no auth header, absolute
  /// URL; otherwise the hfy path with the grant (client.js:123-131,
  /// P-CLIENT-011). No retry on any status — one PUT, then the typed error
  /// with client.js's exact message 'upload failed: `<status>`'.
  Future<void> putUpload(
      Uri url, String mime, Stream<List<int>> bytes, int? size,
      {required bool withAuth}) async {
    final token = gate.token;
    final headers = <String, String>{'content-type': mime};
    if (withAuth && token != null) headers['authorization'] = 'Bearer $token';
    if (size != null) headers['content-length'] = '$size';
    final res = await _dioRequest('PUT', url.toString(),
        headers: headers, bodyBytes: bytes, responseType: ResponseType.bytes);
    final status = _status(res);
    if (status < 200 || status >= 300) {
      throw HfyHttpException(status, 'upload failed: $status');
    }
  }

  /// POST /v1/conversation/approvals `{call_id, approve: !!approve,
  /// result?, answers?}` — optional keys omitted when undefined
  /// (client.js:138-143, P-CLIENT-012).
  Future<Map<String, Object?>> approve(String callId, bool approve,
      {Object? result, Object? answers}) {
    final body = <String, Object?>{'call_id': callId, 'approve': approve};
    if (result != null) body['result'] = result;
    if (answers != null) body['answers'] = answers;
    return _request('/v1/conversation/approvals', method: 'POST', body: body);
  }

  /// POST /v1/conversation/stop with no body (client.js:145-147).
  Future<Map<String, Object?>> stop() =>
      _request('/v1/conversation/stop', method: 'POST');

  /// GET /v1/conversation/config -> {uploads: null | {types, max_bytes}}.
  Future<Map<String, Object?>> config() => _request('/v1/conversation/config');

  // -- SSE ------------------------------------------------------------------

  /// The RequestOptions for ONE SSE open (P-CLIENT-026/033).
  ///
  /// - [token] rides the Authorization header (see the file docs).
  /// - [after] puts `?after=<id>` in the query — the FIRST connection only
  ///   (the history page's live_after; the zero uuid means 'from the
  ///   beginning', events.go:144-146).
  /// - [lastEventId] always rides the `Last-Event-ID` header (events.go:133
  ///   reads the header before `?after`; either mechanism resumes).
  ///
  /// The client runs this on a session-owned Dio (see HfyClient's
  /// _SseSession): a cancelled stream then closes its socket via
  /// adapter.close(force: true) without touching the caller's [dio]. dio's
  /// own post-completion CancelToken abort is unreliable there — the adapter
  /// aborts via a WeakReference that can already be gone.
  RequestOptions eventsOptions(
      {required String token, String? after, required String? lastEventId}) {
    final headers = <String, String>{'authorization': 'Bearer $token'};
    final query = <String, String>{};
    if (lastEventId != null) headers['last-event-id'] = lastEventId;
    if (after != null) query['after'] = after;
    final url = Uri(
        scheme: base.scheme,
        host: base.host,
        port: base.port,
        userInfo: base.userInfo,
        pathSegments: [...base.pathSegments, 'v1', 'conversation', 'events'],
        queryParameters: query.isEmpty ? null : query);
    return RequestOptions(
      path: url.toString(),
      method: 'GET',
      headers: headers,
      responseType: ResponseType.stream,
      validateStatus: (_) => true,
    );
  }

  // -- request core (client.js #requestOnce + readBody) ---------------------

  Future<Map<String, Object?>> _request(String path,
      {String method = 'GET',
      Map<String, Object?>? body,
      bool retryOn401 = true}) async {
    if (gate.token == null && !closed) await gate.fetch('initial');
    final res = await _jsonRequest(method, base.resolve(path).toString(),
        body);
    final status = _status(res);
    final parsed = _readBody(_bodyText(res));
    if (status == 401 && retryOn401 && !closed) {
      await gate.fetch('unauthorized'); // one refresh, one retry
      onUnauthorizedRefresh?.call();
      return _request(path, method: method, body: body, retryOn401: false);
    }
    if (status < 200 || status >= 300) {
      throw HfyHttpException.fromStatus(status, parsed,
          fallback: 'HTTP $status');
    }
    return _asObject(parsed);
  }

  Future<Response<dynamic>> _jsonRequest(
      String method, String url, Map<String, Object?>? body) {
    final headers = <String, String>{};
    final token = gate.token;
    if (token != null) headers['authorization'] = 'Bearer $token';
    if (body != null) headers['content-type'] = 'application/json';
    return _dioRequest(method, url,
        headers: headers,
        bodyText: body == null ? null : jsonEncode(body),
        responseType: ResponseType.plain);
  }

  Future<Response<dynamic>> _dioRequest(String method, String url,
      {Map<String, String>? headers,
      String? bodyText,
      Stream<List<int>>? bodyBytes,
      required ResponseType responseType}) {
    return dio.fetch<dynamic>(
      RequestOptions(
        path: url,
        method: method,
        headers: headers,
        data: bodyText ?? bodyBytes,
        responseType: responseType,
        validateStatus: (_) => true,
      ),
    );
  }

  static int _status(Response<dynamic> res) => res.statusCode ?? 0;

  static String _bodyText(Response<dynamic> res) {
    final data = res.data;
    if (data is String) return data;
    if (data is List<int>) return String.fromCharCodes(data);
    return '';
  }

  // client.js readBody (319-329) + P-CLIENT-023: empty -> null, JSON failure
  // -> {error: text}, everything else the parsed value.
  static Object? _readBody(String text) {
    if (text.isEmpty) return null;
    try {
      return jsonDecode(text);
    } on FormatException {
      return <String, Object?>{'error': text};
    }
  }

  // Callers read properties, so any non-object body degrades to `{}` —
  // exactly what client.js property access on such a value does.
  static Map<String, Object?> _asObject(Object? parsed) {
    if (parsed is Map) return Map<String, Object?>.from(parsed);
    return const <String, Object?>{};
  }
}

/// History accessors over one decoded page + the client.js capture rules
/// (P-CLIENT-006/018): `next`/`live_after` must be strings to count, and
/// `viewer` is captured tolerantly like client.js's typeof-object check.
class HfyHistoryPage {
  final String? next;
  final String? liveAfter;

  const HfyHistoryPage(this.next, this.liveAfter);

  static HfyHistoryPage of(Map<String, Object?> page) {
    String? str(Object? v) => v is String && v.isNotEmpty ? v : null;
    return HfyHistoryPage(str(page['next']), str(page['live_after']));
  }

  /// Returns the page's viewer actor, or null when absent/malformed —
  /// client.js:188 captures only on a successful typeof-object check.
  static HfyActor? viewerOf(Object? viewer) {
    if (viewer is! Map) return null;
    try {
      return HfyActor.fromJson(viewer);
    } on HfyProtocolException {
      return null;
    }
  }

  static List<Object?> turnsOf(Map<String, Object?> page) {
    final turns = page['turns'];
    return turns is List ? turns : const <Object?>[];
  }

  static List<Object?> eventsOf(Object? turn) {
    final events = (turn as Map?)?['events'];
    return events is List ? events : const <Object?>[];
  }
}
