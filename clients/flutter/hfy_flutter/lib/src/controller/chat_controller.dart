// HfyChatController: the state and commands of the embedded chat component
// (internal/httpapi/embed/ui.js connect/retry/loadOlder/post/doStop/
// sendDecisions and the syncDerived outlets) over one HfyClient and one
// HfyConversation. Flutter foundation only: ChangeNotifier and ValueNotifier.
//
// History vs live: the client fires both through `events` without a tag. A
// delivery is LIVE when it moved the cursor, i.e. `event.id == lastEventId`
// while the listener runs (HfyDedupe.claim never moves it for history). Null
// ids count as live. History outside loadOlder folds per microtask burst with
// applyAll (one relayout, like connect()); loadOlder collects its page and
// merges it once with mergeOlderPage (ui.js:1220-1244).
//
// Notifications coalesce: each change marks the controller dirty and one
// microtask notifies, so a burst of events costs one rebuild. Nothing notifies
// after dispose().
import 'dart:async'
    show
        Completer,
        StreamController,
        StreamSubscription,
        scheduleMicrotask,
        unawaited;

import 'package:dio/dio.dart' show Dio;
import 'package:flutter/foundation.dart'
    show ChangeNotifier, ValueNotifier, visibleForTesting;
import 'package:flutter/widgets.dart' show WidgetsBinding;

import '../client/client.dart'
    show HfyAttachmentRef, HfyClient, HfyClientOptions, HfyUploadsConfig;
import '../client/connection.dart' show HfyConnectionState;
import '../client/errors.dart' show HfyHttpException;
import '../l10n/l10n.dart' show HfyL10n;
import '../model/event.dart' show HfyEvent, HfyProtocolException;
import '../model/fold.dart' show HfyConversation;
import '../model/state.dart'
    show HfyBlock, HfyToolCalls, HfyTurn, HfyTz, HfyViewer;
import 'lifecycle.dart';
import 'message.dart';

/// Presentation options, captured once at construction (P-CFG-020: no
/// configure race natively).
class HfyChatOptions {
  const HfyChatOptions({
    this.toolCalls = HfyToolCalls.summary,
    this.base,
    this.tz = const HfyTz.local(),
    this.lang = 'en',
    this.strings = const <String, String>{},
    this.uploads,
  });

  /// Tool-call visibility mode (config.tool_calls, P-CFG-015).
  final HfyToolCalls toolCalls;

  /// Resolves relative attachment urls (P-FOLD-020). [HfyChatController.open]
  /// falls back to the api base when this is null.
  final Uri? base;

  /// The zone for day keys and times (P-CFG-023).
  final HfyTz tz;

  /// The language of the localizable error strings (P-CFG-008).
  final String lang;

  /// Host string overrides, checked before the tables (P-CFG-007).
  final Map<String, String> strings;

  /// Attachment limits (P-CFG-004); null means attachments are impossible.
  final HfyUploadsConfig? uploads;
}

class HfyChatController extends ChangeNotifier {
  /// Drives an existing [client]. Dispose closes it (the controller is its
  /// only consumer), it does not dispose it.
  HfyChatController(
    HfyClient client, {
    HfyChatOptions options = const HfyChatOptions(),
  }) : this._(client, options: options, base: options.base, ownsClient: false);

  /// Builds and owns the client: dispose disposes it.
  factory HfyChatController.open({
    required Uri api,
    required Future<Object?> Function(String reason) tokenProvider,
    HfyChatOptions options = const HfyChatOptions(),
    HfyClientOptions clientOptions = const HfyClientOptions(),
    Dio? dio,
  }) {
    final client = HfyClient(
      api: api,
      tokenProvider: tokenProvider,
      options: clientOptions,
      dio: dio,
    );
    return HfyChatController._(
      client,
      options: options,
      base: options.base ?? api,
      ownsClient: true,
    );
  }

  HfyChatController._(
    this._client, {
    required HfyChatOptions options,
    required Uri? base,
    required bool ownsClient,
  }) : _options = options,
       _base = base,
       _ownsClient = ownsClient,
       _l10n = HfyL10n(
         lang: options.lang,
         custom: options.strings,
         tz: options.tz,
       ),
       stickToBottom = ValueNotifier<bool>(true),
       newMessagesPending = ValueNotifier<bool>(false) {
    _previousProtocolError = _client.onProtocolError;
    _client.onProtocolError = _onProtocolError;
    stickToBottom.addListener(_onStickChanged);
    _client.state.addListener(_onConnectionChanged);
    _eventSub = _client.events.listen(_onClientEvent);
  }

  final HfyClient _client;
  final HfyChatOptions _options;
  final Uri? _base;
  final bool _ownsClient;
  final HfyL10n _l10n;

  /// Forced true by every send (P-FOLD-056) and written by the scroll
  /// position (P-FOLD-045); a send also clears [newMessagesPending].
  final ValueNotifier<bool> stickToBottom;

  /// Set by a live event folded while detached (P-FOLD-018); never by history.
  final ValueNotifier<bool> newMessagesPending;

  late final StreamSubscription<HfyEvent> _eventSub;
  void Function(HfyProtocolException error)? _previousProtocolError;
  final StreamController<HfyProtocolException> _protocol =
      StreamController<HfyProtocolException>.broadcast();

  HfyConversation? _conv;

  /// History delivered outside loadOlder, folded per microtask burst.
  final List<HfyEvent> _pendingHistory = <HfyEvent>[];
  bool _historyQueued = false;

  /// Non-null while loadOlder is collecting its page.
  List<HfyEvent>? _olderPage;
  Future<void>? _olderFlight;
  bool _loadingOlder = false;
  HfyMessage? _loadOlderError;

  /// The send bridge (ui.js _awaitTurn) while no conversation exists yet.
  String? _pendingBridge;

  String _draft = '';
  HfyMessage? _composerError;
  HfyMessage? _connectionError;

  /// Calls whose approval POST succeeded; hidden from the sheet until the
  /// fold drops them (the `submitted` flag of ui.js:1110, held here because
  /// the fold has no setter for it).
  final Set<String> _submitted = <String>{};
  List<HfyBlock> _pending = const <HfyBlock>[];
  bool _sheetOpen = false;
  bool _actionPill = false;
  bool _sheetBusy = false;
  HfyMessage? _sheetError;
  String? _sheetErrorKey;

  int _liveSeq = 0;
  bool _dirty = false;
  bool _notifying = false;
  bool _disposed = false;
  bool _superDisposeDeferred = false;
  HfyLifecycleBinder? _lifecycle;

  // -- readable state ---------------------------------------------------------

  /// The folded turns, oldest first.
  List<HfyTurn> get turns => _conv?.turns ?? const <HfyTurn>[];

  /// Bumped on every fold change (HfyConversation.revision).
  int get revision => _conv?.revision ?? 0;

  /// Live events the fold acted on; the widget compares it to decide when a
  /// detached view should show the new-messages pill.
  int get liveEventSeq => _liveSeq;

  HfyConnectionState get connection => _client.state.value;

  /// The last connect()/retry() failure, cleared at the next attempt.
  HfyMessage? get connectionError => _connectionError;

  bool get showWelcome => _conv?.showWelcome ?? false;
  bool get hasOlder => _conv?.hasOlder ?? false;
  bool get loadingOlder => _loadingOlder;
  HfyMessage? get loadOlderError => _loadOlderError;

  /// The viewer's own turn is queued, running or awaiting approval, or a send
  /// bridge still waits for its turn (P-FOLD-022). Drives Stop mode.
  bool get ownActive {
    final conv = _conv;
    return conv != null ? conv.ownActive : _pendingBridge != null;
  }

  /// Send enabled: not disposed, not ownActive, a non-blank draft (P-CMP-003).
  bool get canSend => !_disposed && !ownActive && _draft.trim().isNotEmpty;

  /// Stop enabled while the viewer's own turn is active (P-CMP-002, P-CMP-014).
  bool get canStop => !_disposed && ownActive;

  /// Attachments are impossible without uploads config and blocked like
  /// sending while the own turn runs (P-CFG-014).
  bool get attachDisabled => _options.uploads == null || _disposed || ownActive;

  HfyUploadsConfig? get uploads => _options.uploads;

  String get draft => _draft;
  HfyMessage? get composerError => _composerError;

  /// The viewer's open approvals in call order, minus the ones already
  /// submitted (P-FOLD-042).
  List<HfyBlock> get pendingOwn => _pending;

  /// The approval sheet is open while calls pend and it is not minimized
  /// (P-FOLD-049).
  bool get sheetOpen => _sheetOpen;

  /// The "Action needed" pill while the sheet is minimized (P-SH-015).
  bool get showActionPill => _actionPill;

  bool get sheetBusy => _sheetBusy;
  HfyMessage? get sheetError => _sheetError;

  /// The step the sheet lands on after a failed submit: `<callId>` or
  /// `<callId>:<questionId>` (P-FOLD-055).
  String? get sheetErrorKey => _sheetErrorKey;

  /// The text a message shows: its server text, or the localized key.
  String textOf(HfyMessage message) =>
      message.text ?? _l10n.t(message.key ?? 'error');

  Stream<HfyProtocolException> get protocolErrors => _protocol.stream;

  // -- commands ---------------------------------------------------------------

  /// connect() (ui.js:856-858, P-CFG-021): waits for an in-flight loadOlder,
  /// boots the client, then marks the conversation loaded and reads hasOlder.
  /// A failure is recorded in [connectionError], never thrown.
  Future<void> connect() async {
    if (_disposed) return;
    await _settleOlder();
    if (_disposed) return;
    _connectionError = null;
    _markDirty();
    try {
      await _client.connect();
      _ensureConv().markLoaded();
    } on Object catch (error) {
      _connectionError = _messageFor(error);
    }
    if (_disposed) return;
    _flushHistory();
    _conv?.setHasOlder(_client.hasOlder);
    _markDirty();
  }

  /// ui.js:861-866 (P-CFG-022): a no-op while live, else connect() again.
  Future<void> retry() async {
    if (_disposed || _client.state.value == HfyConnectionState.live) return;
    await connect();
  }

  /// ui.js:1210-1248: one older page, collected then merged once. Errors are
  /// kept in [loadOlderError] (the web swallows them, P-TL-003).
  Future<void> loadOlder() async {
    if (_disposed || _loadingOlder || !_client.hasOlder) return;
    final done = Completer<void>();
    final page = <HfyEvent>[];
    _olderFlight = done.future;
    _olderPage = page;
    _loadingOlder = true;
    _loadOlderError = null;
    _markDirty();
    try {
      final more = await _client.loadOlder();
      _olderPage = null;
      if (!_disposed) _ensureConv().mergeOlderPage(page, hasMore: more);
    } on Object catch (error) {
      _olderPage = null;
      if (!_disposed) _loadOlderError = _messageFor(error);
    } finally {
      _loadingOlder = false;
      _olderFlight = null;
      done.complete();
      _markDirty();
    }
  }

  void setDraft(String text) {
    if (_disposed || text == _draft) return;
    _draft = text;
    _composerError = null;
    _markDirty();
  }

  /// The composer's Send (ui.js sendMsg): trims, clears the draft before the
  /// request, and on failure restores the text only while the draft is still
  /// empty and shows the error (P-FOLD-047, P-CMP-004, P-CMP-013).
  Future<void> submitDraft() async {
    if (!canSend) return;
    final text = _draft.trim();
    _draft = '';
    _markDirty();
    final error = await send(text);
    if (_disposed || error == null) return;
    if (_draft.isEmpty) _draft = text;
    _composerError = error;
    _markDirty();
  }

  /// ui.js post(): forces stick-to-bottom, sends, and on success clears the
  /// composer error and arms the send bridge on the returned turn (P-FOLD-056,
  /// P-CMP-016). Returns null on success, else the message to show. It never
  /// sets [composerError] to an error, so the preview path can route its own.
  Future<HfyMessage?> send(
    String text, {
    List<HfyAttachmentRef>? attachments,
  }) async {
    if (_disposed) return const HfyMessage.key('error');
    stickToBottom.value = true;
    try {
      final turn = await _client.send(
        text,
        attachments: attachments == null
            ? null
            : <Map<String, Object?>>[for (final a in attachments) a.toWire()],
      );
      if (_disposed) return null;
      _composerError = null;
      if (turn != null && turn.isNotEmpty) {
        final conv = _conv;
        if (conv == null) {
          _pendingBridge = turn;
        } else {
          conv.awaitTurn(turn);
        }
      }
      _markDirty();
      return null;
    } on Object catch (error) {
      return _messageFor(error);
    }
  }

  /// ui.js doStop (1460-1464): drops the send bridge at once, posts stop, and
  /// keeps a failure as the composer error.
  Future<void> stop() async {
    if (_disposed) return;
    _pendingBridge = null;
    _conv?.clearAwaitTurn();
    _markDirty();
    try {
      await _client.stop();
      if (!_disposed) _composerError = null;
    } on Object catch (error) {
      if (!_disposed) _composerError = _messageFor(error);
    }
    _markDirty();
  }

  /// ui.js sendDecisions (1635-1658, P-FOLD-055): posts in list order, stops
  /// at the first failure, and lands the sheet on the failing step.
  Future<void> decide(List<HfyDecision> decisions) =>
      _runDecisions(decisions, landOnFailure: true);

  /// ui.js skipConfirmed (1662-1671, P-SH-013): declines one call's questions.
  /// A failure shows the error without landing the sheet.
  Future<void> skipQuestions(String callId) => _runDecisions(<HfyDecision>[
    HfyDecision(callId: callId, approve: false),
  ], landOnFailure: false);

  /// ui.js clearStepError (1583): only an active landing is cleared.
  void clearSheetError() {
    if (_sheetErrorKey == null) return;
    _sheetErrorKey = null;
    _sheetError = null;
    _markDirty();
  }

  void minimizeSheet() {
    if (_disposed) return;
    _conv?.minimizeSheet();
    _markDirty();
  }

  void openSheet() {
    if (_disposed) return;
    _conv?.openSheet();
    _markDirty();
  }

  /// Native lifecycle pair (P-NAT-004); forwarded to the client.
  void pause() {
    if (!_disposed) _client.pause();
  }

  void resume() {
    if (!_disposed) _client.resume();
  }

  /// Pauses the stream in the background and resumes it on return
  /// (P-NAT-004). The registration ends at [dispose].
  void attachLifecycle(WidgetsBinding binding) {
    if (_disposed) return;
    (_lifecycle ??= HfyLifecycleBinder(
      onPause: pause,
      onResume: resume,
    )).attach(binding);
  }

  @visibleForTesting
  bool get lifecycleAttached => _lifecycle?.attached ?? false;

  @override
  void dispose() {
    if (_disposed) return;
    _disposed = true;
    _lifecycle?.detach();
    unawaited(_eventSub.cancel());
    _client.state.removeListener(_onConnectionChanged);
    stickToBottom.removeListener(_onStickChanged);
    _client.onProtocolError = _previousProtocolError;
    _pendingHistory.clear();
    _olderPage = null;
    _pending = const <HfyBlock>[];
    _submitted.clear();
    unawaited(_protocol.close());
    stickToBottom.dispose();
    newMessagesPending.dispose();
    if (_ownsClient) {
      unawaited(_disposeOwnedClient());
    } else {
      _client.close();
    }
    // ChangeNotifier forbids dispose() during its own notification; a
    // listener that disposes us gets the base dispose after the loop ends.
    if (_notifying) {
      _superDisposeDeferred = true;
    } else {
      super.dispose();
    }
  }

  // -- events -----------------------------------------------------------------

  void _onClientEvent(HfyEvent event) {
    if (_disposed) return;
    final id = event.id;
    if (id == null || id == _client.lastEventId) {
      _onLive(event);
    } else {
      _onHistory(event);
    }
  }

  void _onLive(HfyEvent event) {
    _flushHistory();
    if (!_ensureConv().apply(event)) return;
    _liveSeq++;
    if (!stickToBottom.value) newMessagesPending.value = true;
    _markDirty();
  }

  void _onHistory(HfyEvent event) {
    final page = _olderPage;
    if (page != null) {
      page.add(event);
      return;
    }
    _pendingHistory.add(event);
    if (!_historyQueued) {
      _historyQueued = true;
      scheduleMicrotask(_flushHistory);
    }
  }

  void _flushHistory() {
    _historyQueued = false;
    if (_pendingHistory.isEmpty || _disposed) return;
    final batch = List<HfyEvent>.of(_pendingHistory);
    _pendingHistory.clear();
    final conv = _ensureConv();
    final before = conv.revision;
    conv.applyAll(batch);
    if (conv.revision != before) _markDirty();
  }

  void _onStickChanged() {
    if (stickToBottom.value) newMessagesPending.value = false;
  }

  void _onConnectionChanged() => _markDirty();

  void _onProtocolError(HfyProtocolException error) {
    _previousProtocolError?.call(error);
    if (!_protocol.isClosed) _protocol.add(error);
  }

  // -- internals ----------------------------------------------------------------

  /// The conversation is created on first need, after the client has captured
  /// the viewer from the history page (client.js #viewer, P-CLIENT-006).
  HfyConversation _ensureConv() {
    final existing = _conv;
    if (existing != null) return existing;
    final viewer = _client.viewer;
    final conv = HfyConversation(
      viewer: viewer == null
          ? null
          : HfyViewer(uuid: viewer.uuid, type: viewer.type, ref: viewer.ref),
      toolCalls: _options.toolCalls,
      base: _base,
      tz: _options.tz,
      fileLabel: _l10n.t('file'),
    );
    _conv = conv;
    final bridge = _pendingBridge;
    if (bridge != null) {
      _pendingBridge = null;
      conv.awaitTurn(bridge);
    }
    return conv;
  }

  Future<void> _settleOlder() async {
    final flight = _olderFlight;
    if (flight != null) await flight;
  }

  Future<void> _runDecisions(
    List<HfyDecision> decisions, {
    required bool landOnFailure,
  }) async {
    if (_disposed || _sheetBusy) return;
    _sheetBusy = true;
    _sheetError = null;
    _sheetErrorKey = null;
    _markDirty();
    String? failKey;
    for (final decision in decisions) {
      try {
        await _client.approve(
          decision.callId,
          decision.approve,
          answers: decision.answers,
        );
        if (_disposed) return;
        _submitted.add(decision.callId);
      } on Object catch (error) {
        final message = _messageFor(error);
        _sheetError = message;
        if (landOnFailure) failKey = _failKey(decision, message);
        break;
      }
    }
    _sheetBusy = false;
    _sheetErrorKey = failKey;
    _markDirty();
  }

  /// A failure lands on the question the server names, else the first
  /// question of the set, else on the call (ui.js:1648-1649).
  String _failKey(HfyDecision decision, HfyMessage message) {
    if (decision.questionIds.isEmpty) return decision.callId;
    final named = _namedQuestion(message.text ?? '', decision.questionIds);
    return '${decision.callId}:${named ?? decision.questionIds.first}';
  }

  /// The first question id (in set order) that appears in [text] as a whole
  /// word. A plain search, not a regex built from the id (ui.js:1648 builds
  /// one, which breaks on special characters). An empty id never names one.
  String? _namedQuestion(String text, List<String> questionIds) {
    for (final id in questionIds) {
      if (id.isEmpty) continue;
      var at = text.indexOf(id);
      while (at >= 0) {
        final end = at + id.length;
        if (!_isWordAt(text, at - 1) && !_isWordAt(text, end)) return id;
        at = text.indexOf(id, at + 1);
      }
    }
    return null;
  }

  HfyMessage _messageFor(Object error) {
    if (error is HfyHttpException && error.message.isNotEmpty) {
      return HfyMessage.text(error.message);
    }
    return const HfyMessage.key('error');
  }

  /// Recomputes the sheet view from the fold and the submitted set, then
  /// schedules one notification. Every state change goes through here.
  void _markDirty() {
    if (_disposed) return;
    _refreshPending();
    if (_dirty) return;
    _dirty = true;
    scheduleMicrotask(_flush);
  }

  void _flush() {
    if (_dirty && !_disposed) {
      _dirty = false;
      _notifying = true;
      try {
        notifyListeners();
      } finally {
        _notifying = false;
      }
    }
    if (_superDisposeDeferred) {
      _superDisposeDeferred = false;
      super.dispose();
    }
  }

  void _refreshPending() {
    final conv = _conv;
    if (conv == null) {
      _pending = const <HfyBlock>[];
      _submitted.clear();
    } else {
      final open = conv.pendingOwn.toSet();
      _submitted.removeWhere((callId) => !open.contains(callId));
      final list = <HfyBlock>[];
      for (final turn in conv.turns) {
        if (!turn.own) continue;
        for (final block in turn.blocks) {
          if (block.isTool &&
              block.showApproval &&
              !_submitted.contains(block.callId)) {
            list.add(block);
          }
        }
      }
      _pending = list;
    }
    if (_pending.isEmpty) {
      _sheetError = null;
      _sheetErrorKey = null;
    }
    final minimized = _conv?.showActionPill ?? false;
    _sheetOpen = _pending.isNotEmpty && !minimized;
    _actionPill = _pending.isNotEmpty && minimized;
  }

  Future<void> _disposeOwnedClient() async {
    try {
      await _client.dispose();
    } on Object catch (_) {
      // The controller is gone: nobody is left to hear a failed teardown.
    }
  }
}

final RegExp _identifierChar = RegExp(r'[\p{L}\p{N}_]', unicode: true);

bool _isWordAt(String text, int index) =>
    index >= 0 && index < text.length && _identifierChar.hasMatch(text[index]);
