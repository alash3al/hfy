// HfyClient — the Dart port of the embedded web client's protocol unit
// (internal/httpapi/embed/client.js), PARITY.md §1 + P-NAT-002:
//   - connect() (no-op when a stream is open, closed-flag reset, token ->
//     history page 1 -> pin live_after -> open SSE -> schedule refresh ->
//     live; failure -> error + rethrow) — P-CLIENT-004/008/018/034;
//   - loadOlder(): one history page per call over the `next` cursor
//     (P-CLIENT-009/019);
//   - send/upload/approve/stop (P-CLIENT-010..013);
//   - close(): timers cancelled, stream closed, state `ended`; after that no
//     events fire, no 401 retry and no token fetch (P-CLIENT-014/016);
//   - pause()/resume(): NEW native-only lifecycle pair (P-NAT-004 analog);
//     pause() before the first connect() is a documented no-op (review item
//     9), resume() defers to an in-flight connect() boot (review item 8);
//   - SSE via dio streams with a fresh HfySseParser per connection, the
//     HfyIdleWatchdog fed at connect time and on EVERY byte chunk (>30s of
//     silence = dead), end-of-stream => fresh token + reconnect resuming
//     from lastEventId through the [1000,2000,5000] ms ladder, and
//     HfyProtocolException (parser/envelope) => reconnect instead of a crash;
//   - a LIVE envelope HfyEvent.fromJson REJECTS (actor not an object, a `v`
//     that is not an int, …) is dropped WITHOUT the reconnect loop: its id
//     is claimed in the dedupe/cursor so the resume moves past it, and the
//     failure surfaces only through the onProtocolError diagnostic (review
//     item 3 — the server replays the same event, so reconnecting here
//     would trap the client in an endless 1s loop);
//   - proactive refresh with the client.js skew formula
//     `max(expires − now − min(skew, remaining/2), 1000)` (P-CLIENT-024/025);
//   - envelope dedupe (HfyDedupe) gates everything listeners receive:
//     every id exactly once, live only advances lastEventId (P-CLIENT-015).
//
// Deviations from client.js, all native-only and intentional (see PARITY.md /
// the final phase report): the state enum adds `paused`; the SSE token rides
// the Authorization header instead of `?token=` (dio can set headers); the
// token provider is single-flight; one connect() boot is shared between
// racing callers (review item 8, unlike client.js's double boot).
import 'dart:async' show StreamController, StreamSubscription, Timer, unawaited;
import 'dart:typed_data' show Uint8List;

import 'package:dio/dio.dart'
    show
        BaseOptions,
        Dio,
        HttpClientAdapter,
        RequestOptions,
        ResponseBody;
import 'package:dio/io.dart' show IOHttpClientAdapter;
import 'package:flutter/foundation.dart' show ValueListenable, ValueNotifier;

import '../model/event.dart' show HfyActor, HfyEvent, HfyProtocolException;
import 'api.dart' show HfyApi, HfyEventResponse, HfyHistoryPage, HfyTokenGate;
import 'connection.dart';
import 'dedupe.dart' show HfyDedupe;
import 'sse.dart' show HfyIdleWatchdog, HfySseMessage, HfySseParser;

const String _kZeroUuid = '00000000-0000-0000-0000-000000000000';

Timer _realTimerFn(Duration delay, void Function() callback) =>
    Timer(delay, callback);

const HfyTimerFactory _kRealTimerFactory = _realTimerFn;

const List<Duration> _kBackoff = <Duration>[
  Duration(milliseconds: 1000),
  Duration(milliseconds: 2000),
  Duration(milliseconds: 5000),
];

int _systemClock() => DateTime.now().millisecondsSinceEpoch;

/// Client tuning: client.js options plus the native-only knobs of P-NAT-002.
class HfyClientOptions {
  /// Proactive refresh this long before expiry; client.js refreshSkewMs
  /// defaults to 30000 (P-CLIENT-005).
  final Duration refreshSkew;

  /// P-NAT-002: NO bytes for longer than this means a dead stream. The server
  /// pings every 15s, so default 30s = two missed pings.
  final Duration idleTimeout;

  /// Reconnect ladder; client.js [1000,2000,5000] ms, last step repeating
  /// forever (P-CLIENT-030).
  final List<Duration> reconnectBackoff;

  /// `Date.now()` in epoch ms; injectable for fake-clock tests.
  final HfyClock clock;

  /// One factory for the refresh/reconnect timers (client.js timer(fn, ms)).
  final HfyTimerFactory timerFactory;

  /// The idle watchdog's timer factory; defaults to [timerFactory]. Separate
  /// so tests can fire the watchdog without disturbing the ladder.
  final HfyTimerFactory idleTimerFactory;

  /// Builds the HttpClientAdapter of EVERY SSE session's own Dio (review
  /// item 4). The session builds its Dio from the caller Dio's BaseOptions
  /// (timeouts, headers template, extra, queryParameters) and copies ALL its
  /// interceptors, so proxy/TLS/test wired in Dart must see the SSE open.
  /// The ADAPTER always stays session-owned and session-closed
  /// (adapter.close(force: true) at cancel — the author-verified socket
  /// teardown), so the factory must return a FRESH adapter per call;
  /// unchanged (null): a fresh IOHttpClientAdapter copying the caller
  /// adapter's createHttpClient/validateCertificate knobs.
  final HttpClientAdapter Function()? sseAdapterFactory;

  const HfyClientOptions({
    this.refreshSkew = const Duration(milliseconds: 30000),
    this.idleTimeout = const Duration(milliseconds: 30000),
    List<Duration>? reconnectBackoff,
    HfyClock? clock,
    HfyTimerFactory? timerFactory,
    HfyTimerFactory? idleTimerFactory,
    this.sseAdapterFactory,
  })  : reconnectBackoff = reconnectBackoff ?? _kBackoff,
        clock = clock ?? _systemClock,
        timerFactory = timerFactory ?? _kRealTimerFactory,
        idleTimerFactory = idleTimerFactory ?? timerFactory ?? _kRealTimerFactory;
}

/// An attachment reference as the wire carries it (`{file, mime, name?}` —
/// client.js upload's return shape, P-CLIENT-011 / P-CFG-026).
class HfyAttachmentRef {
  final String file;
  final String mime;
  final String? name;

  const HfyAttachmentRef({required this.file, required this.mime, this.name});

  /// The wire map for send(attachments:) — keys in the reference.md order.
  Map<String, Object?> toWire() => <String, Object?>{
        'file': file,
        'mime': mime,
        if (name != null) 'name': name,
      };

  @override
  String toString() => 'HfyAttachmentRef(file: $file, mime: $mime)';
}

/// GET /v1/conversation/config's answer (uploads normalization per P-CFG-004:
/// non-empty types array and max_bytes > 0, else null = attachments
/// impossible).
class HfyClientConfig {
  final HfyUploadsConfig? uploads;

  const HfyClientConfig({this.uploads});

  factory HfyClientConfig.fromWire(Object? uploads) {
    if (uploads is! Map) return const HfyClientConfig();
    final types = uploads['types'];
    final maxBytes = uploads['max_bytes'];
    if (types is! List || types.isEmpty) return const HfyClientConfig();
    if (maxBytes is! num || maxBytes <= 0) return const HfyClientConfig();
    return HfyClientConfig(
      uploads: HfyUploadsConfig(
          types: List<String>.from(types), maxBytes: maxBytes.toInt()),
    );
  }
}

class HfyUploadsConfig {
  final List<String> types;
  final int maxBytes;

  const HfyUploadsConfig({required this.types, required this.maxBytes});
}

/// The protocol unit of hfy_flutter: broadcast streams ([events], [state])
/// plus promise methods. Construct with the api base and a token provider,
/// then drive with connect().
class HfyClient {
  /// The all-zero event id means "from the beginning" (events.go:144-146):
  /// the first SSE open sends it when the history page had no live_after.
  static const String kZeroEventId = _kZeroUuid;

  final HfyClientOptions options;
  final HfyApi _wire;
  final HfyDedupe _dedupe = HfyDedupe();

  /// Every accepted envelope, broadcast, after dedupe (P-CLIENT-015).
  ///
  /// A SYNCHRONOUS broadcast controller (client.js fires listeners
  /// synchronously, P-CLIENT-007: unsubscribe mid-dispatch is safe), and an
  /// ordinary Dart stream: `.first` / `firstWhere` / `await for` /
  /// pause+resume all behave like the platform promises (review item 1 —
  /// the deleted custom stream broke exactly those).
  ///
  /// LISTENER ERRORS: a listener's own exception NEVER aborts the
  /// connection or the other listeners — per Dart's stream rules the
  /// broadcast delivery isolates each subscription: the others still get
  /// their events in the same dispatch, LATER frames still arrive, the
  /// cursor already advanced and resume() still opens past the gap; the
  /// exception surfaces through the listener's own zone (the same
  /// Zone.current.handleUncaughtError path every Dart listener callback
  /// takes). Wrap the client's listen() calls in a zone with
  /// handleUncaughtError when the listener can throw.
  final StreamController<HfyEvent> _events = StreamController<HfyEvent>.broadcast(sync: true);

  final ValueNotifier<HfyConnectionState> _state =
      ValueNotifier<HfyConnectionState>(HfyConnectionState.idle);

  /// True from close() to the next connect(): no fires (P-CLIENT-014), no
  /// 401 retry, no token fetch; connect() resets it (P-CLIENT-034).
  bool _closed = false;

  /// The native-only pause() flag (P-NAT-004 analog).
  bool _paused = false;

  /// Whether connect() was ever started (the resume() gate).
  bool _everStarted = false;

  String? _nextCursor; // the history `next` cursor (client.js #nextCursor)
  HfyActor? _viewer;
  Timer? _refreshTimer;
  Timer? _reconnectTimer;
  int _reconnectAttempts = 0;
  _SseSession? _stream;

  /// The ONE in-flight connect() boot; racing callers re-await it instead of
  /// double-loading history / double-opening SSE (review item 8).
  Future<void>? _connectFlight;

  /// Set by dispose(): any state move after that is a programming error and
  /// fails with a typed StateError (not a Flutter assert deep inside the
  /// notifier).
  bool _disposed = false;

  /// The Dio this client created itself (dispose() closes it; an injected
  /// dio stays the caller's property and is never closed here).
  final Dio? _ownedDio;

  /// Diagnostics for malformed envelopes (review item 3): never reconnects,
  /// never throws, only reported. See [_fire].
  void Function(HfyProtocolException error)? onProtocolError;

  factory HfyClient({
    required Uri api,
    required Future<Object?> Function(String reason) tokenProvider,
    HfyClientOptions options = const HfyClientOptions(),
    Dio? dio,
    void Function(HfyProtocolException error)? onProtocolError,
  }) {
    // One Dio total for the null case: the wire and the owned field share it.
    final theDio = dio ?? Dio();
    return HfyClient._(
      wire: HfyApi(
        dio: theDio,
        base: _stripSlashes(api),
        gate: HfyTokenGate(tokenProvider, clock: options.clock),
      ),
      ownedDio: dio == null ? theDio : null,
      options: options,
      onProtocolError: onProtocolError,
    );
  }

  HfyClient._({
    required HfyApi wire,
    required Dio? ownedDio,
    required this.options,
    required this.onProtocolError,
  })  : _wire = wire,
        _ownedDio = ownedDio {
    // client.js:217 — a refreshed-on-401 token reschedules the refresh timer.
    _wire.onUnauthorizedRefresh = _scheduleRefresh;
  }

  static Uri _stripSlashes(Uri api) {
    var path = api.path;
    while (path.length > 1 && path.endsWith('/')) {
      path = path.substring(0, path.length - 1);
    }
    return api.replace(path: path);
  }

  // -- readable state (client.js:72-77, P-CLIENT-006) -----------------------

  /// The connection state machine (client.js values + the native-only
  /// paused/); listeners fire only on changes (P-CLIENT-031).
  ValueListenable<HfyConnectionState> get state => _state;

  /// Every accepted envelope, broadcast, after dedupe (P-CLIENT-015).
  Stream<HfyEvent> get events => _events.stream;

  /// The resumption cursor: the newest LIVE envelope id delivered, else the
  /// live_after pin of the last connect() (client.js #lastEventId).
  String? get lastEventId => _dedupe.lastEventId;

  /// One or more older history pages remain for loadOlder().
  bool get hasOlder => _nextCursor != null;

  /// The grant's own actor, known once the first history page lands
  /// (client.js #viewer; captured before events fire, P-CLIENT-006/018).
  HfyActor? get viewer => _viewer;

  // -- promise methods -----------------------------------------------------

  /// client.js:89-106 (P-CLIENT-008): no-op when a stream is already open;
  /// otherwise the full boot with the closed-flag reset (P-CLIENT-034).
  /// Racing callers share the ONE in-flight boot (review item 8); a later
  /// connect() after that boot simply no-ops on the open stream.
  Future<void> connect() {
    if (_stream != null) return Future<void>.value();
    final inFlight = _connectFlight;
    if (inFlight != null) return inFlight;
    final flight = _connect();
    _connectFlight = flight;
    // Side chain that clears the slot; its errors are already delivered to
    // every awaiter of [flight].
    unawaited(flight.then((_) {
      _clearFlight(flight);
    }, onError: (Object e, StackTrace st) {
      _clearFlight(flight);
    }));
    return flight;
  }

  void _clearFlight(Future<void> flight) {
    if (identical(_connectFlight, flight)) _connectFlight = null;
  }

  Future<void> _connect() async {
    _everStarted = true;
    _closed = false; // a close()d client can be driven again (P-CLIENT-034)
    _paused = false;
    _wire.closed = false;
    _setState(HfyConnectionState.connecting);
    try {
      await _wire.gate.fetch('initial');
      final liveAfter = await _loadHistory(null);
      // client.js:96-97: live_after wins over any last fired id — it is the
      // server's end mark of the history page. Pinned BEFORE the paused
      // check (review item 7): pause() during the boot must still leave the
      // cursor at live_after, otherwise resume() reopens from the all-zero
      // id and replays the whole conversation.
      if (liveAfter != null) _dedupe.pinLiveAfter(liveAfter);
      if (_paused) return; // pause() raced the token/history awaits: sleep
      _openStream();
      _scheduleRefresh();
      _reconnectAttempts = 0;
      _setState(HfyConnectionState.live);
    } catch (err) {
      if (!_closed && !_paused) _setState(HfyConnectionState.error);
      rethrow;
    }
  }

  /// client.js:108-112 (P-CLIENT-009): without a cursor this returns false
  /// WITHOUT a request; otherwise exactly one older page and whether one
  /// more remains. Each call fetches ONE page (the UI chips call it again).
  Future<bool> loadOlder() async {
    final cursor = _nextCursor;
    if (cursor == null) return false;
    await _loadHistory(cursor);
    return _nextCursor != null;
  }

  /// POST /v1/conversation/messages `{text}` + `attachments` only when
  /// defined (P-CLIENT-010); the 202 body's `turn` is the new turn id.
  Future<String?> send(String text,
      {List<Map<String, Object?>>? attachments}) async {
    final res = await _wire.sendMessage(text, attachments);
    return res['turn'] as String?;
  }

  /// The upload flow of client.js:120-135 / uploads.go (P-CLIENT-011): POST
  /// {mime, size} for an upload slot, then PUT the bytes where the slot
  /// directs. `direct: true` (presigned S3) = the absolute URL with NO auth
  /// header; otherwise the hfy path WITH the grant. A non-2xx PUT surfaces
  /// as HfyHttpException('upload failed: `<status>`').
  ///
  /// [mime]/[size] are the file's type/size; the returned [HfyAttachmentRef]
  /// carries [name] only when given (a Blob has no name — client.js:133).
  Future<HfyAttachmentRef> upload({
    required String mime,
    required int size,
    String? name,
    required Stream<List<int>> bytes,
  }) async {
    final slot = await _wire.requestUploadSlot(mime, size);
    final file = slot['file'];
    final uploadUrl = slot['upload_url'];
    if (file is! String || uploadUrl is! String) {
      throw const HfyProtocolException('upload slot lacks file/upload_url');
    }
    final direct = slot['direct'] == true;
    // A non-direct slot carries the bearer: HfyApi.uploadUri REJECTS an
    // absolute URL there instead of putting the grant on a foreign host
    // (review item 2, uploads.go:100 always produces a server path).
    final putUrl = _wire.uploadUri(uploadUrl, direct: direct);
    await _wire.putUpload(putUrl, mime, bytes, size, withAuth: !direct);
    return HfyAttachmentRef(file: file, mime: mime, name: name);
  }

  /// POST /v1/conversation/approvals `{call_id, approve, result?, answers?}`
  /// — optional keys omitted when undefined (P-CLIENT-012).
  Future<void> approve(String callId, bool approve,
      {Object? result, Object? answers}) async {
    await _wire.approve(callId, approve, result: result, answers: answers);
  }

  /// POST /v1/conversation/stop, no body (P-CLIENT-013).
  Future<void> stop() async {
    await _wire.stop();
  }

  /// GET /v1/conversation/config (P-CFG-004's client side).
  Future<HfyClientConfig> config() async {
    return HfyClientConfig.fromWire((await _wire.config())['uploads']);
  }

  /// client.js:149-155 (P-CLIENT-014): everything stops, ends. After this no
  /// events fire, no 401 retry and no token fetch happen; the next connect()
  /// revives the client (P-CLIENT-034).
  void close() {
    _closed = true;
    _wire.closed = true;
    _cancelRefresh();
    _cancelReconnectTimer();
    _closeStream();
    _setState(HfyConnectionState.ended);
  }

  /// NEW (P-NAT-004 analog): close the SSE stream and cancel the timers
  /// WITHOUT ending the client. Idempotent; a later wave wires this to
  /// WidgetsBindingObserver.didAppLifecycleChange(paused).
  ///
  /// A pause() BEFORE the first connect() is a documented NO-OP (review item
  /// 9): the client never started, so a `paused` state there could never be
  /// left again (resume() gates on having boot-connected once).
  void pause() {
    if (_paused || _closed || !_everStarted) return;
    _paused = true;
    _cancelRefresh();
    _cancelReconnectTimer();
    _closeStream();
    _setState(HfyConnectionState.paused);
  }

  /// NEW (P-NAT-004 analog): reconnect from lastEventId through the normal
  /// reconnect path (P-CLIENT-030). Idempotent; no-op when the client never
  /// connected (resume() only runs resume-after-pause) or after close()
  /// (connect() is the revival instead, P-CLIENT-034).
  ///
  /// While a connect() boot is in flight, resume() DEFERS to it (review item
  /// 8): the boot reopens the stream on its own; the resumed `connecting`
  /// state owns it. This also covers pause()-then-resume() DURING that boot:
  /// the flight's paused-check re-reads the cleared flag and still pins.
  void resume() {
    if (!_paused || _closed || !_everStarted) return;
    _paused = false;
    if (_stream != null) return; // a connect() raced us; it owns the stream
    final inFlight = _connectFlight;
    if (inFlight != null) {
      unawaited(inFlight.then(
        (_) {},
        onError: (Object e, StackTrace st) {},
      ));
      return;
    }
    _setState(HfyConnectionState.reconnecting);
    unawaited(_runReconnect());
  }

  /// Everything stops AND the client gives up its resources (review item
  /// 1/9): the event controller closes (listeners get onDone), the state
  /// notifier disposes, and a Dio the client created ITSELF is closed
  /// (an injected one belongs to the caller). After dispose() the client
  /// must no longer be driven — and, unlike [close], a dispose()d client
  /// cannot merely be revived by another connect(); every further state
  /// move fails with a typed StateError. Build a new client instead.
  Future<void> dispose() async {
    close(); // the final `ended` state move happens BEFORE the disposal
    _disposed = true;
    _connectFlight = null;
    await _events.close();
    _state.dispose();
    _ownedDio?.close();
  }

  // -- internals: state ----------------------------------------------------

  void _setState(HfyConnectionState value) {
    // ValueNotifier notifies only on change (client.js #setState guard,
    // P-CLIENT-031's "no listener fires for unchanged states").
    if (_disposed) {
      throw StateError('HfyClient was dispose()d; build a new client');
    }
    _state.value = value;
  }

  // -- internals: history firing (client.js #fire + #loadHistory) ----------

  /// Fires one envelope through dedupe into the broadcast stream. History
  /// envelopes are seeded without moving the cursor (P-CLIENT-015).
  void _fire(Object? envelope, {required bool isHistory}) {
    if (_closed || envelope == null) return;
    final HfyEvent event;
    try {
      event = HfyEvent.fromJson(envelope);
    } on HfyProtocolException catch (error) {
      _dropRejectedEnvelope(envelope, isHistory, error);
      return;
    }
    if (!_dedupe.claim(event.id, isHistory: isHistory)) return; // replayed
    // Synchronous broadcast: every listener gets it now, a throwing one is
    // isolated by the stream itself (see [events]) and THE NEXT FRAME of
    // this same chunk is still processed afterwards (review item 1).
    if (!_events.isClosed) _events.add(event);
  }

  /// A typed model rejection: the envelope is dead data (review item 3).
  /// The server REPLAYS the same envelope, so a reconnect ladder here would
  /// loop forever with a successful open resetting it — instead the
  /// envelope's id (if any) is CLAIMED in the dedupe/cursor so resume()
  /// moves past it, the frame is dropped, delivery of later frames
  /// continues, and the failure surfaces only through [onProtocolError]
  /// (diagnostic only).
  void _dropRejectedEnvelope(
      Object? envelope, bool isHistory, HfyProtocolException error) {
    final id =
        envelope is Map && envelope['id'] is String ? envelope['id'] as String : null;
    _dedupe.claim(id, isHistory: isHistory);
    onProtocolError?.call(error);
  }

  /// One decoded history page: capture cursor + viewer BEFORE firing, then
  /// fire oldest-first as-is (P-CLIENT-006/018/019). Returns live_after.
  Future<String?> _loadHistory(String? before) async {
    final page = await _wire.history(before: before);
    final meta = HfyHistoryPage.of(page);
    _nextCursor = meta.next;
    // client.js:188: viewer captured BEFORE firing so listeners can read it;
    // a malformed one keeps the previous (client.js's typeof-object guard).
    final viewer = HfyHistoryPage.viewerOf(page['viewer']);
    if (viewer != null) _viewer = viewer;
    for (final turn in HfyHistoryPage.turnsOf(page)) {
      for (final envelope in HfyHistoryPage.eventsOf(turn)) {
        _fire(envelope, isHistory: true);
      }
    }
    return meta.liveAfter;
  }

  // -- internals: SSE (client.js #openStream + P-NAT-002) ------------------

  /// Opens a fresh SSE connection; every open closes the previous stream
  /// (client.js #openStream first calls #closeStream). The HTTP work runs in
  /// the background (EventSource is equally asynchronous): connect() and the
  /// refresh turn `live` immediately, open failures arrive as reconnects.
  void _openStream() {
    _closeStream();
    final session = _SseSession(
      client: this,
      watchdog: HfyIdleWatchdog(
        idle: options.idleTimeout,
        onDead: _onStreamDead,
        timerFactory: options.idleTimerFactory,
      ),
      dio: _newSessionDio(),
    );
    _stream = session;
    session.start(_wire.eventsOptions(
      token: _wire.gate.token ?? '',
      after: _dedupe.lastEventId ?? _kZeroUuid,
      lastEventId: _dedupe.lastEventId ?? _kZeroUuid,
    ));
  }

  void _closeStream() {
    final stream = _stream;
    _stream = null;
    stream?.cancel();
  }

  /// The SSE session's throwaway Dio built from the CALLER's dio (review
  /// item 4): BaseOptions copied, ALL interceptors copied, and its own
  /// HttpClientAdapter — a fresh one from [options.sseAdapterFactory] or an
  /// IOHttpClientAdapter clone with the caller's createHttpClient /
  /// validateCertificate knobs (TLS pinning must ride along). The session
  /// closes its OWN adapter (safe, it shares nothing); the caller's adapter
  /// and its keep-alive sockets are never touched.
  Dio _newSessionDio() {
    final template = _wire.dio;
    final dio = Dio(_sessionBaseOptions(template.options));
    dio.interceptors.addAll(template.interceptors);
    dio.httpClientAdapter = options.sseAdapterFactory?.call() ??
        _cloneSseAdapter(template.httpClientAdapter);
    return dio;
  }

  static BaseOptions _sessionBaseOptions(BaseOptions template) => BaseOptions(
        baseUrl: template.baseUrl,
        connectTimeout: template.connectTimeout,
        sendTimeout: template.sendTimeout,
        receiveTimeout: template.receiveTimeout,
        queryParameters: Map<String, dynamic>.of(template.queryParameters),
        headers: Map<String, dynamic>.of(template.headers),
        extra: Map<String, dynamic>.of(template.extra),
        contentType: template.contentType,
        persistentConnection: template.persistentConnection,
        preserveHeaderCase: template.preserveHeaderCase,
        followRedirects: template.followRedirects,
        maxRedirects: template.maxRedirects,
        listFormat: template.listFormat,
        responseDecoder: template.responseDecoder,
        requestEncoder: template.requestEncoder,
        // The SSE open OVERRIDES base headers per request (dio merges), so an
        // app's own 'authorization' template never clobbers the token.
      );

  // shortcut: only IOHttpClientAdapter is cloned field-wise; any other
  // adapter type falls back to a default IOHttpClientAdapter — pass
  // HfyClientOptions.sseAdapterFactory when the app must run a custom
  // adapter for SSE.
  static HttpClientAdapter _cloneSseAdapter(HttpClientAdapter template) {
    if (template is IOHttpClientAdapter) {
      return IOHttpClientAdapter(
        createHttpClient: template.createHttpClient,
        validateCertificate: template.validateCertificate,
      );
    }
    return IOHttpClientAdapter();
  }

  /// The stream's headers arrived — the Dart analog of EventSource's 'open'
  /// event: the ladder restarts at the first step (P-CLIENT-029). Surrogate
  /// for the browser's own reconnection logic (P-DIFF-005/009).
  void _onStreamOpen(_SseSession session, HfyEventResponse response) {
    if (_stream != session) {
      session.cancel(); // superseded while in flight
      return;
    }
    if (response.status < 200 || response.status >= 300) {
      // A non-2xx open is an EventSource error with readyState CLOSED in the
      // web: an unauthorized refresh, then one ladder reopen (P-CLIENT-030).
      // A 3xx belongs here too: followRedirects is off, so a token-stealing
      // or proxy redirect shows up as a non-2xx status, not a silent hop
      // (review item 11).
      session.cancel();
      _scheduleReconnect();
      return;
    }
    _reconnectAttempts = 0;
    session.listen(response.body.stream);
    session.watchdog.feed(); // armed AT CONNECT TIME, then per byte chunk
  }

  void _onStreamBytes(_SseSession session, List<int> bytes) {
    if (_stream != session) return;
    session.watchdog.feed(); // pings count: ANY byte is life (P-NAT-002)
    List<HfySseMessage> frames;
    try {
      frames = session.parser.push(bytes);
    } on HfyProtocolException {
      // The parser's size limits (and any framing break) => reconnect ladder,
      // never a crash (phase task: "parse errors/HfyProtocolException =>
      // reconnect, surface via state").
      session.cancel();
      _scheduleReconnect();
      return;
    }
    // Each frame is fired separately, so ONE malformed envelope (dropped by
    // _fire) cannot orphan the frames that follow it in the same chunk
    // (review item 3), and a throwing LISTENER cannot orphan them either
    // (review item 1).
    for (final frame in frames) {
      _fireFrame(frame);
    }
  }

  void _fireFrame(HfySseMessage frame) {
    // HfyEvent.tryJson never throws; null means unparseable => P-CLIENT-027's
    // silently-dropped frame (no envelope exists, so there is no id to
    // claim — P-CLIENT-027 stays the contract there).
    final envelope = HfyEvent.tryJson(frame.data);
    _fire(envelope, isHistory: false);
  }

  /// Stream end (the server closed it: grant ended or revoked) => a fresh
  /// token, then one ladder reconnect resuming from lastEventId
  /// (P-NAT-002; the EventSource CLOSED-readyState path).
  void _onStreamDone(_SseSession session) {
    if (_stream != session) return;
    _stream = null;
    session.cancel();
    _scheduleReconnect();
  }

  void _onStreamError(_SseSession session) {
    if (_stream != session) return;
    _stream = null;
    session.cancel();
    _scheduleReconnect();
  }

  /// The idle watchdog's verdict: no bytes for options.idleTimeout.
  void _onStreamDead() {
    _closeStream();
    _scheduleReconnect();
  }

  // -- internals: refresh timers (client.js #scheduleRefresh/#refreshStream)

  void _cancelRefresh() {
    _refreshTimer?.cancel();
    _refreshTimer = null;
  }

  /// client.js:230-239 (P-CLIENT-024): any prior timer cancelled first;
  /// skipped when closed or the expiry is unknown; the delay is
  /// `max(remaining − min(skew, remaining/2), 1000)` ms.
  void _scheduleRefresh() {
    _cancelRefresh();
    if (_closed || _paused) return;
    final expiresAtMs = _wire.gate.expiresAtMs;
    if (expiresAtMs == null) return; // no expiry: no proactive refresh
    final remaining = expiresAtMs - options.clock();
    final skew = options.refreshSkew.inMilliseconds;
    final head = remaining ~/ 2 < skew ? remaining ~/ 2 : skew;
    final delay = remaining - head < 1000 ? 1000 : remaining - head;
    _refreshTimer = options.timerFactory(Duration(milliseconds: delay), () {
      _refreshTimer = null;
      unawaited(_refreshStream());
    });
  }

  /// client.js:241-254 (P-CLIENT-025): refreshing -> token (reason expired)
  /// -> reopen the stream -> reschedule -> attempts reset -> live; token
  /// failure -> the reconnect ladder (unless closed).
  Future<void> _refreshStream() async {
    if (_closed || _paused) return;
    _setState(HfyConnectionState.refreshing);
    try {
      await _wire.gate.fetch('expired');
      if (_closed || _paused) return;
      _openStream();
      _scheduleRefresh();
      _reconnectAttempts = 0;
      _setState(HfyConnectionState.live);
    } catch (err) {
      if (!_closed && !_paused) _scheduleReconnect();
    }
  }

  // -- internals: the reconnect ladder (client.js #scheduleReconnect) ------

  void _cancelReconnectTimer() {
    _reconnectTimer?.cancel();
    _reconnectTimer = null;
  }

  /// client.js:296-316 (P-CLIENT-030): closed/already-scheduled => nothing;
  /// close the stream, state `reconnecting`, delay = ladder[attempt], then a
  /// fresh token (reason `unauthorized`) → reopen → live. The token failure
  /// reschedules (client.js:313), capped at the last step forever.
  void _scheduleReconnect() {
    if (_closed || _paused || _reconnectTimer != null) return;
    _closeStream();
    _setState(HfyConnectionState.reconnecting);
    final steps = options.reconnectBackoff;
    final delay = steps[_reconnectAttempts < steps.length - 1
        ? _reconnectAttempts
        : steps.length - 1];
    _reconnectAttempts++;
    _reconnectTimer = options.timerFactory(delay, () {
      _reconnectTimer = null;
      if (_closed || _paused) return;
      unawaited(_runReconnect());
    });
  }

  /// The common body of the reconnect timer (client.js:303-315) and of
  /// resume(): a fresh token, a reopened stream, a refresh reschedule, live.
  Future<void> _runReconnect() async {
    if (_closed || _paused) return;
    try {
      await _wire.gate.fetch('unauthorized');
      if (_closed || _paused) return;
      _openStream();
      _scheduleRefresh();
      _setState(HfyConnectionState.live);
    } catch (err) {
      _scheduleReconnect(); // backs off again, capped at the last step
    }
  }
}

/// One open SSE connection: fresh parser, idle watchdog, stream subscription
/// and cancellation — client.js's `eventSource` field under a Dart shape.
/// Stale sessions' callbacks verify `client._stream == this` before moving
/// anything (client.js's `this.#eventSource !== es` guards).
///
/// Its dio is built per open from the CALLER's dio's BaseOptions + ALL
/// interceptors, with its own HttpClientAdapter (see HfyClientOptions
/// .sseAdapterFactory): the app's proxy/TLS/test wiring must see the SSE
/// request, while the force-close teardown that reliably kills THIS socket
/// (adapter.close(force: true) — dio's shared CancelToken post-completion
/// abort is a WeakReference that can already be gone) must NOT tear the
/// caller's keep-alive sockets. HfyClient._newSessionDio documents exactly
/// what is copied (BaseOptions and ALL interceptors) and how the adapter is
/// cloned fresh per session (review item 4).
class _SseSession {
  final HfyClient client;
  final HfyIdleWatchdog watchdog;
  final HfySseParser parser = HfySseParser();
  final Dio _dio;
  StreamSubscription<Uint8List>? _sub;

  _SseSession({
    required this.client,
    required this.watchdog,
    required Dio dio,
  }) : _dio = dio;

  /// Connects; the response headers route to the client when they exist and
  /// failures/cancellations alike route to the client's ladder.
  void start(RequestOptions options) {
    unawaited(_dio
        .fetch<ResponseBody>(options)
        .then((response) => client._onStreamOpen(
            this,
            HfyEventResponse(response.statusCode ?? 0, response.data!)))
        .catchError((Object error, StackTrace stackTrace) {
      client._onStreamError(this);
      return null; // consumed: never an unhandled error
    }));
  }

  void listen(Stream<Uint8List> bytes) {
    _sub = bytes.listen((Uint8List chunk) => client._onStreamBytes(this, chunk),
        onDone: () => client._onStreamDone(this),
        onError: (Object error, StackTrace stackTrace) =>
            client._onStreamError(this));
  }

  /// Kills THIS stream's socket for good (force-close), whomever initiated
  /// the teardown. Superseded/in-flight opens are cancelled likewise: the
  /// adapter-level close aborts both the hung open and the streaming body.
  void cancel() {
    _sub?.cancel();
    _sub = null;
    watchdog.cancel();
    _dio.httpClientAdapter.close(force: true);
  }
}
