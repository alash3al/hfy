// The conversation fold: a pure-Dart port of the embedded UI's fold,
// relayout and syncDerived (internal/httpapi/embed/ui.js:958-1050, 1085-1187,
// 788-809) plus the loadOlder collect/merge (ui.js:1210-1248). It folds wire
// envelopes (lib/src/model/event.dart) into turns and blocks and matches the
// shared fixture projections (clients/fixtures).
//
// Non-obvious rules cite ui.js lines; the fixture contract is
// clients/fixtures/README.md. Flutter-agnostic: no widgets, no dart:io.

import 'event.dart';
import 'ask.dart';
import 'state.dart';
import 'values.dart';

/// Private turn record — mutable, like ui.js's _ensureTurn object (ui.js:868).
class _Turn implements HfyTurn {
  _Turn(this.id);

  @override
  final String id;
  @override
  Object? agent = '';
  @override
  String state = 'queued'; // created lazily in this state (ui.js:870)
  @override
  bool stateSeen = false;
  @override
  Object? error;
  @override
  bool own = false;
  @override
  bool showTyping = false;
  @override
  String userLabel = '';
  @override
  final List<_Row> blocks = <_Row>[];
}

/// Private block record — every field the template reads lives on ONE class,
/// like ui.js _baseBlock (889-906), so a late event can fill any field in
/// place. Instances are exposed only through the [HfyBlock] read interface.
class _Row implements HfyBlock {
  _Row({
    required this.id,
    required this.kind,
    required this.isAgent,
    required this.ref,
    required this.actorUuid,
    required this.actorKey,
    required this.ts,
    required this.iso,
    required this.dayKey,
  });

  @override
  String id;
  @override
  final String kind;
  @override
  final bool isAgent;
  @override
  final String ref;
  @override
  final String actorUuid;
  @override
  final String actorKey;
  @override
  int ts;
  @override
  String iso;
  @override
  String dayKey;

  @override
  Object? text = '';
  @override
  List<HfyAttachment> images = const <HfyAttachment>[];
  @override
  List<HfyAttachment> files = const <HfyAttachment>[];
  @override
  bool streaming = false;
  bool _ownValue = false;
  @override
  bool get own => _ownValue;

  // tool fields (defaults on text/reasoning rows — one block class, ui.js)
  @override
  String callId = '';
  @override
  String name = '';
  @override
  String status = '';
  @override
  Object? args;
  @override
  Object? content = '';
  @override
  Object? structured;
  @override
  bool isAsk = false;
  @override
  Object? answers;
  @override
  HfyAskState? askState;
  @override
  String decision = '';
  @override
  String declineReason = '';
  @override
  bool stopped = false;
  @override
  bool showApproval = false;
  @override
  bool showDecided = false;
  @override
  bool submitted = false;
  @override
  Object? choice;
  @override
  Object? card;
  @override
  List<HfyAskQuestion> questions = const <HfyAskQuestion>[];

  // relayout-time fields (P-FOLD-024/027)
  @override
  bool showDay = false;
  @override
  bool showHead = false;
  @override
  bool hidden = false;
  @override
  bool showSummary = false;
  @override
  bool showDetails = false;
  @override
  bool showPendingOwn = false;
  @override
  bool showPendingOther = false;
  @override
  bool cardButtonsLive = false;

  @override
  bool get isText => kind == 'text';
  @override
  bool get isReasoning => kind == 'reasoning';
  @override
  bool get isTool => kind == 'tool';
  @override
  bool get isUser => !isAgent;
  @override
  bool get isRunning => status == 'running';
  @override
  bool get hasQuestions => questions.isNotEmpty;
}

/// The conversation fold + derived state. Feed every envelope through
/// [apply]; the read side mirrors the embed component's fields ([turns],
/// [pendingOwn], [sheetOpen], [showActionPill], [ownActive], [showWelcome])
/// so the fixture projections and the UI phases read the same state.
class HfyConversation {
  HfyConversation({
    required this.viewer,
    this.toolCalls = HfyToolCalls.summary,
    this.base,
    HfyClock? clock,
    this.tz = const HfyTz.utc(),
  }) : clock = clock ?? const HfySystemClock();

  /// The grant's actor; own-ness compares its uuid (P-FOLD-021).
  final HfyViewer? viewer;

  /// The tool-call visibility mode (config.tool_calls, P-CFG-015).
  final HfyToolCalls toolCalls;

  /// Page-origin stand-in for `new URL(raw, location.href)` (ui.js:308-311):
  /// relative attachment urls resolve against it (P-FOLD-020).
  final Uri? base;

  final HfyClock clock;
  final HfyTz tz;

  /// The `File` fallback of attachment labels (ui.js:933's t('file') with the
  /// en strings table; localized tables land in Phase 2).
  String fileLabel = 'File';

  final List<_Turn> _turns = <_Turn>[];

  /// Folded turns, oldest first (ui.js keeps them push/prepend ordered;
  /// loadOlder puts older ones in front).
  List<HfyTurn> get turns => List.unmodifiable(_turns);

  /// `_collecting` (ui.js:1045): history events fold here while a loadOlder
  /// page is in flight; [mergeOlder] applies the merge.
  List<_Turn>? _collecting;

  bool _loaded = false; // ui.js:856 (`this._loaded = true` after connect)
  bool _ownActive = false;
  bool _showWelcome = false;

  // approval sheet state (ui.js:1480-1536 syncSheet)
  List<_Row> _pendingOwn = const <_Row>[];
  bool _sheetOpen = false;
  bool _showActionPill = false;
  bool _sheetMin = false;
  final Set<String> _seenPending = <String>{};
  String?
  _awaitTurn; // ui.js:799: bridges send() and the turn's first user text

  // ---- public derived state --------------------------------------------------

  /// `showWelcome` (ui.js:795, P-FOLD-046). NOTE it reads the COMPONENT's
  /// hasOlder — only a loadOlder round moves that, never the client's
  /// initial pointer.
  bool get showWelcome => _showWelcome;

  /// Stop-mode (ui.js:799-801, P-FOLD-022): the viewer's own turn is
  /// queued/running/awaiting_approval, or a send's turn id still bridges.
  bool get ownActive => _ownActive;

  /// The embed component.s hasOlder mirror (ui.js:857/1241). The client push
  /// duty: [mergeOlder] during paging, or an assignment followed by syncDerived.
  bool hasOlder = false;

  /// The viewer's open approvals in folded call order (ui.js:1110, P-FOLD-042)
  /// — question sets included; the sheet separates them by hasQuestions.
  List<String> get pendingOwn => [for (final b in _pendingOwn) b.callId];

  /// The sheet is open while calls pend and not minimized (ui.js:1532).
  bool get sheetOpen => _sheetOpen;

  /// "Action needed" pill (ui.js:1498): sheet minimized while calls pend.
  bool get showActionPill => _showActionPill;

  // ---- lifecycle -------------------------------------------------------------

  /// `_loaded = true`, mirroring `await hfyClient.connect()` succeeding
  /// (ui.js:856). Until then showWelcome stays down (P-FOLD-046).
  void markLoaded() => _loaded = true;

  /// The sheet's minimize affordance (ui.js:1605): only `_sheetMin` moves;
  /// the caller still owns one sync to settle the flags.
  void minimizeSheet() {
    _sheetMin = true;
    syncSheet();
  }

  /// Reopening the minimized sheet (ui.js:1606).
  void openSheet() {
    _sheetMin = false;
    syncSheet();
  }

  /// The send() bridge (ui.js:1287's `_awaitTurn`): show Stop until the turn
  /// owns a user text block (ui.js:799-800) — a fixture-driven wave never
  /// sets it; the controller phase does.
  void awaitTurn(String turnId) => _awaitTurn = turnId;

  /// ui.js:1462's doStop clears the bridge at once.
  void clearAwaitTurn() => _awaitTurn = null;

  // ---- the fold (ui.js:958-1050) ----------------------------------------------

  /// One envelope through `fold(env, isHistory)`. Live deliveries fold into
  /// [turns] with relayout + sync; history folds there too EXCEPT while a
  /// loadOlder collection is open ([applyHistoryPage]), where they fold into
  /// the collector with no layout at all (ui.js:1045).
  void apply(HfyEvent e, {bool history = false}) {
    final r = _foldInto(
      history && _collecting != null ? _collecting! : _turns,
      e,
    );
    if (!r.hands) return; // unknown type (ui.js:1043) — dropped, no layout
    // loadOlder merges and lays out once (ui.js:1045).
    if (history && _collecting != null) return;
    if (r.layout) relayout();
    syncDerived();
  }

  /// `loadOlder()`'s first half (ui.js:1220-1223): open the collector and
  /// fold one fetched page. DEDUPE stays client-side — the caller applies
  /// HfyDedupe#claim before handing envelopes here.
  void applyHistoryPage(List<HfyEvent> page) {
    _collecting ??= <_Turn>[];
    for (final e in page) {
      _foldInto(_collecting!, e);
    }
  }

  /// `loadOlder()`'s second half (ui.js:1224-1248): match the collected turns
  /// against the live ones, append only unseen block ids, overwrite the state
  /// ONLY when the collected turn actually saw a turn.state event, settle
  /// anything the merge drags under a terminal state, prepend the brand-new
  /// turns, then one relayout + sync over the final list.
  void mergeOlder({required bool hasMore}) {
    final collected = _collecting ?? const <_Turn>[];
    _collecting = null;
    final byId = <String, _Turn>{for (final t in _turns) t.id: t};
    final prepend = <_Turn>[];
    for (final ct in collected) {
      final existing = byId[ct.id];
      if (existing == null) {
        prepend.add(ct); // a turn the live page never carried
        continue;
      }
      final seenBlocks = <String>{for (final b in existing.blocks) b.id};
      for (final b in ct.blocks) {
        if (seenBlocks.add(b.id)) existing.blocks.add(b);
      }
      // only a collected turn that actually saw a turn.state event may
      // overwrite state (ui.js:1234)
      if (ct.stateSeen && ct.state.isNotEmpty) existing.state = ct.state;
      if (hfyTruthy(ct.error)) existing.error = ct.error;
      // a merge can drag in untouched asks while restoring a terminal state:
      // they settle like any other end of turn, idempotently (ui.js:1238)
      if (kHfyTerminalStates.contains(existing.state)) _settleCalls(existing);
    }
    _turns.insertAll(0, prepend);
    hasOlder = hasMore; // ui.js:1241
    relayout();
    syncDerived();
  }

  /// One presentation pass (ui.js:1085-1172): grouping, typing dots, ask
  /// states and the tool visibility matrix. Recomputed from scratch every
  /// call — cheap, and how the JS fold stays consistent.
  void relayout() {
    var prevKey = '';
    var prevDay = '';
    var prevTs = 0;
    var firstActive = true;
    final pending = <_Row>[];
    for (final turn in _turns) {
      final terminal = kHfyTerminalStates.contains(turn.state);
      // a turn is the viewer's when its first user text block's actor uuid is
      // the viewer's (ui.js:1095-1098, P-FOLD-021)
      _Row? starter;
      for (final b in turn.blocks) {
        if (b.isText && b.isUser) {
          starter = b;
          break;
        }
      }
      turn.own =
          starter != null &&
          viewer != null &&
          hfyTruthy(viewer!.uuid) &&
          starter.actorUuid == viewer!.uuid;
      turn.userLabel = starter == null ? '' : _actorLabelFor(starter);
      for (final b in turn.blocks) {
        // a terminal turn switches its streaming blocks off (ui.js:1100)
        if (terminal && b.streaming) b.streaming = false;
        var hidden = false;
        if (b.isTool) {
          // pending approvals stay visible in every mode; the viewer's own
          // open the sheet, others' are a read-only note (ui.js:1104-1109)
          final hasQuestions = b.hasQuestions;
          b.showPendingOwn = b.showApproval && turn.own && !hasQuestions;
          b.showPendingOther = b.showApproval && !turn.own && !hasQuestions;
          if (b.showApproval && turn.own && !b.submitted) pending.add(b);
          if (hasQuestions) b.askState = _askRecord(b, turn.own);
          // visibility matrix (ui.js:1126-1131, P-FOLD-024): ask calls are
          // never tool rows, hidden mode keeps decided approvals, cards keep
          // the block alive in every mode
          b.showSummary =
              !hasQuestions &&
              !b.showApproval &&
              (toolCalls == HfyToolCalls.summary ||
                  (toolCalls == HfyToolCalls.hidden && b.decision.isNotEmpty));
          b.showDetails =
              !hasQuestions &&
              !b.showApproval &&
              toolCalls == HfyToolCalls.details;
          hidden =
              !hasQuestions &&
              !b.showApproval &&
              !b.showSummary &&
              !b.showDetails &&
              b.card == null;
          // a question set the server refused (or one not yet paused) has
          // nothing to show (ui.js:1130-1131)
          if (hasQuestions && b.askState == HfyAskState.asking) hidden = true;
          b.hidden = hidden;
        }
        // grouping (P-FOLD-027): showDay marks the day's first VISIBLE block;
        // head marks a day change, an actor change or a text block more than
        // five minutes after the previous visible block of ANY kind
        // (ui.js:1139-1142)
        final own = hfyTruthy(viewer?.uuid) && b.actorUuid == viewer!.uuid;
        final showDay = !hidden && b.dayKey != prevDay;
        final head =
            !hidden &&
            (showDay ||
                b.actorKey != prevKey ||
                (b.isText && b.ts - prevTs > 5 * 60 * 1000));
        if (!hidden) {
          prevDay = b.dayKey;
          prevKey = b.actorKey;
          prevTs = b.ts;
        }
        b._ownValue = own;
        b.showDay = showDay;
        b.showHead = head;
      }
      // typing dots (ui.js:1161-1166, P-FOLD-036): on the first active turn,
      // when its last block isn't already busy; active/awaiting turns consume
      // the firstActive slot
      final active = turn.state == 'queued' || turn.state == 'running';
      _Row? last;
      if (turn.blocks.isNotEmpty) last = turn.blocks.last;
      final busy =
          last != null &&
          ((last.isText && last.isAgent) ||
              (last.isTool &&
                  (last.showApproval ||
                      (last.isRunning && toolCalls != HfyToolCalls.hidden))) ||
              (last.isReasoning && last.streaming));
      turn.showTyping = active && firstActive && !busy;
      if (active || turn.state == 'awaiting_approval') firstActive = false;
    }
    _pendingOwn = pending;
  }

  /// The derived conversation state (ui.js:788-809): welcome, ownActive and
  /// the sheet/card outlets. The embed calls it after every fold, load and
  /// configure.
  void syncDerived() {
    _showWelcome = _loaded && _turns.isEmpty && !hasOlder;
    // _awaitTurn clears once the turn owns a user text block (ui.js:799-800)
    final wait = _awaitTurn;
    if (wait != null) {
      final landed = _turns.any(
        (t) => t.id == wait && t.blocks.any((b) => b.isText && b.isUser),
      );
      if (landed) _awaitTurn = null;
    }
    _ownActive =
        _awaitTurn != null ||
        _turns.any((t) => t.own && kHfyActiveStates.contains(t.state));
    // card liveness reads the FINAL ownActive, never a stale one
    // (ui.js:1179-1187, P-FOLD-041)
    for (var i = 0; i < _turns.length; i++) {
      final turn = _turns[i];
      for (final b in turn.blocks) {
        if (b.isTool) {
          b.cardButtonsLive =
              b.card != null &&
              i == _turns.length - 1 &&
              turn.own &&
              !_ownActive;
        }
      }
    }
    syncSheet();
  }

  /// The sheet's settled outlets (ui.js:1480-1536 minus the wizard's
  /// in-progress steps): open while calls pend and not minimized; a NEW
  /// pending callId always surfaces the sheet again (P-FOLD-049).
  void syncSheet() {
    final list = _pendingOwn;
    var fresh = false;
    for (final b in list) {
      if (_seenPending.add(b.callId)) fresh = true;
    }
    if (fresh) _sheetMin = false; // a new request surfaces the sheet
    _showActionPill = list.isNotEmpty && _sheetMin;
    _sheetOpen = list.isNotEmpty && !_sheetMin;
  }

  // ---- fold internals ---------------------------------------------------------

  /// The switch under ui.js:963-1044 into one target list. Returns whether
  /// the type is handled at all (`hands`) and whether relayout must run
  /// (`layout` — pure delta appends skip it, ui.js:985/991).
  ({bool hands, bool layout}) _foldInto(List<_Turn> list, HfyEvent e) {
    var layout = true;
    final d = e.data;
    // Turn ids: `env.id` for type `turn`, else `env.parent ‖ env.id` (ui.js:961).
    final turnId = e.type == 'turn'
        ? (e.id ?? '')
        : (hfyTruthy(e.parent) ? e.parent! : (e.id ?? ''));
    switch (e.type) {
      case 'turn':
        final turn = _ensureTurn(list, turnId);
        turn.agent = hfyOr(d['agent'], turn.agent);
        break;
      case 'turn.state':
        final turn = _ensureTurn(list, turnId);
        turn.stateSeen = true;
        turn.state = _wireText(hfyOr(d['state'], turn.state), turn.state);
        final err = d['error'];
        if (hfyTruthy(err)) turn.error = err;
        // a turn that ends takes its as-yet-undecided calls with it (ui.js:970)
        if (kHfyTerminalStates.contains(turn.state)) _settleCalls(turn);
        break;
      case 'message.text':
        final turn = _ensureTurn(list, turnId);
        final isAgent = e.actor?.type == 'agent';
        final streaming = isAgent ? _findStreamingBlock(turn, 'text') : null;
        if (streaming != null) {
          // the durable text REPLACES the streaming block in place
          // (ui.js:975-979: text, attachments, id, ts/iso/dayKey restamped,
          // streaming off)
          final nb = _newTextBlock(e, true, d);
          streaming.text = nb.text;
          streaming.images = nb.images;
          streaming.files = nb.files;
          streaming.streaming = false;
          streaming.id = nb.id;
          streaming.ts = nb.ts;
          streaming.iso = nb.iso;
          streaming.dayKey = nb.dayKey;
        } else {
          turn.blocks.add(_newTextBlock(e, isAgent, d));
        }
        break;
      case 'text.delta':
        final turn = _ensureTurn(list, turnId);
        final b = _findStreamingBlock(turn, 'text');
        if (b == null) {
          final fresh = _newTextBlock(e, true, const <String, Object?>{});
          fresh.id = '$turnId:stream'; // ui.js:986
          fresh.streaming = true;
          fresh.text = hfyOr(d['text'], '');
          turn.blocks.add(fresh);
        } else {
          layout = false; // pure delta append: no relayout (ui.js:985)
          b.text = '${hfyString(b.text)}${hfyString(hfyOr(d['text'], ''))}';
        }
        break;
      case 'reasoning.delta':
        final turn = _ensureTurn(list, turnId);
        final b = _findStreamingBlock(turn, 'reasoning');
        if (b == null) {
          final fresh = _baseBlock(e, 'reasoning');
          fresh.id = '$turnId:rstream'; // ui.js:992
          fresh.streaming = true;
          fresh.text = hfyOr(d['text'], '');
          turn.blocks.add(fresh);
        } else {
          layout = false;
          b.text = '${hfyString(b.text)}${hfyString(hfyOr(d['text'], ''))}';
        }
        break;
      case 'reasoning':
        final turn = _ensureTurn(list, turnId);
        final b = _findStreamingBlock(turn, 'reasoning');
        if (b != null) {
          // P-FOLD-058: the durable reasoning event replaces text/streaming/id
          // but NOT ts/iso/dayKey — ui.js:997 only, unlike message.text:977.
          b.text = hfyOr(d['text'], '');
          b.streaming = false;
          b.id = e.id ?? b.id;
        } else {
          final fresh = _baseBlock(e, 'reasoning');
          fresh.text = hfyOr(d['text'], '');
          turn.blocks.add(fresh);
        }
        break;
      case 'tool_call':
        _ensureTurn(list, turnId).blocks.add(_newToolBlock(e, d));
        break;
      case 'tool_result':
        final turn = _ensureTurn(list, turnId);
        var row = _findTool(turn, _wireText(d['call_id'], ''));
        if (row == null) {
          // a tool_result for a call the fold never saw (ui.js:1006)
          row = _newToolBlock(e, {'call_id': d['call_id']});
          turn.blocks.add(row);
        }
        _setToolStatus(row, hfyTruthy(d['is_error']) ? 'error' : 'done');
        row.content = hfyOr(d['content'], '');
        // an ask's result is {"answers": {...}}: the fallback when the
        // decision event carried none (ui.js:1009-1010, P-FOLD-039)
        if (row.isAsk && !hfyTruthy(row.answers)) {
          final res = hfyDataLike(row.content);
          if (res is Map) {
            final a = res['answers'];
            if (hfyTruthy(a) && (a is Map || a is List)) row.answers = a;
          }
        }
        if (d['ui'] != null) row.card = d['ui'];
        if (d['structured'] != null) row.structured = d['structured'];
        // a held call's result arrives without a decision only when the turn
        // took it (ui.js:1013-1015)
        if (row.showApproval && !row.showDecided) _settleCall(row);
        break;
      case 'tool_call.interrupted':
        final turn = _ensureTurn(list, turnId);
        final b = _findTool(turn, _wireText(d['call_id'], ''));
        if (b != null) _setToolStatus(b, 'interrupted');
        break;
      case 'approval.requested':
        final turn = _ensureTurn(list, turnId);
        final b = _findTool(turn, _wireText(d['call_id'], ''));
        _Row row;
        if (b == null) {
          row = _newToolBlock(e, {'call_id': d['call_id']});
          turn.blocks.add(row);
        } else {
          row = b;
        }
        // design B: the request carries the tool's question set (ui.js:1023)
        final rq = d['questions'];
        if (hfyTruthy(rq) && row.questions.isEmpty) {
          row.questions = hfyAskQuestions(rq);
        }
        if (d['ui'] != null) row.card = d['ui'];
        if (!row.showDecided) {
          // a request arriving after the turn's terminal state missed its
          // turn: it settles at once (ui.js:1030-1032)
          if (kHfyTerminalStates.contains(turn.state)) {
            _settleCall(row);
          } else {
            row.showApproval = true;
          }
        }
        break;
      case 'approval.decided':
        final turn = _ensureTurn(list, turnId);
        final b = _findTool(turn, _wireText(d['call_id'], ''));
        if (b != null) {
          b.showApproval = false;
          b.showDecided = true;
          b.decision = hfyTruthy(d['approve']) ? 'allowed' : 'denied';
          final answered = d['answers'];
          if (hfyTruthy(answered) && (answered is Map || answered is List)) {
            b.answers = answered;
          }
          b.declineReason = _wireText(hfyOr(d['reason'], ''), '');
        }
        break;
      default:
        // P-FOLD-057: summary/model.usage (and unknown types) drop here — no
        // block, no layout. Envelopes reach the app through the controller's
        // event stream, never through the fold.
        return (hands: false, layout: false);
    }
    return (hands: true, layout: layout);
  }

  _Turn _ensureTurn(List<_Turn> list, String id) {
    for (final t in list) {
      if (t.id == id) return t;
    }
    final turn = _Turn(id);
    list.add(turn);
    return turn;
  }

  /// `_findTool` (ui.js:873-877).
  _Row? _findTool(_Turn turn, String callId) {
    for (final b in turn.blocks) {
      if (b.isTool && b.callId == callId) return b;
    }
    return null;
  }

  /// `streamingBlock` (ui.js:878-884) — scans backward.
  _Row? _findStreamingBlock(_Turn turn, String kind) {
    for (var i = turn.blocks.length - 1; i >= 0; i--) {
      final b = turn.blocks[i];
      if (b.kind == kind && b.streaming) return b;
    }
    return null;
  }

  /// `_baseBlock` (ui.js:889-906): timestamps parse from `created_at` with a
  /// Date.now() fallback for unparsable stamps (ui.js:891; the fixture
  /// contract demands real RFC3339 stamps, so the fallback stays unused).
  _Row _baseBlock(HfyEvent env, String kind, {bool isAgent = true}) {
    final ts =
        env.createdAt?.millisecondsSinceEpoch ??
        clock.now().millisecondsSinceEpoch;
    final actor = env.actor;
    final actorUuid = actor?.uuid ?? '';
    final iso = DateTime.fromMillisecondsSinceEpoch(
      ts,
      isUtc: true,
    ).toIso8601String();
    return _Row(
      id: env.id ?? '',
      kind: kind,
      isAgent: isAgent,
      ref: actor?.ref ?? '',
      actorUuid: actorUuid,
      // actorKey: 'a:<ref ‖ main>' for agents, 'u:<ref ‖ uuid>' for users
      // (ui.js:893)
      actorKey: isAgent
          ? 'a:${_orFallback(actor?.ref, 'main')}'
          : 'u:${_orFallback(_orFallback2(actor?.ref, actorUuid), '')}',
      ts: ts,
      iso: iso,
      dayKey: tz.dayKey(ts),
    );
  }

  /// `_newTextBlock` (ui.js:913-916).
  _Row _newTextBlock(HfyEvent env, bool isAgent, Map<String, Object?> d) {
    final b = _baseBlock(env, 'text', isAgent: isAgent);
    b.text = hfyOr(d['text'], '');
    final atts = hfyMapAttachments(
      d['attachments'],
      base: base,
      fileLabel: fileLabel,
    );
    b.images = atts
        .where((a) => a.isImage)
        .toList(); // setAtts split (ui.js:907-911)
    b.files = atts.where((a) => !a.isImage).toList();
    return b;
  }

  /// `_newToolBlock` (ui.js:918-926).
  _Row _newToolBlock(HfyEvent env, Map<String, Object?> d) {
    final b = _baseBlock(env, 'tool');
    b.callId = _wireText(d['call_id'], '');
    b.name = _wireText(hfyOr(d['name'], ''), '');
    b.args = d['arguments']; // absent ≡ null after ui.js:921's collapse
    b.isAsk = b.name == kHfyAskTool;
    if (b.isAsk) b.questions = hfyAskQuestions(d['arguments']);
    _setToolStatus(b, 'running');
    return b;
  }

  /// `_setToolStatus` (ui.js:927).
  void _setToolStatus(_Row b, String status) => b.status = status;

  /// `_settleCall` (ui.js:947-950, P-FOLD-016): idempotent and late-safe — a
  /// genuine approval.decided arriving later still overrides (it never
  /// checks `stopped`).
  void _settleCall(_Row b) {
    b.showApproval = false;
    b.showDecided = true;
    b.stopped = true;
    if (b.isRunning) _setToolStatus(b, 'interrupted');
  }

  /// `_settleCalls` (ui.js:951-956).
  void _settleCalls(_Turn turn) {
    for (final b in turn.blocks) {
      if (b.isTool && b.showApproval && !b.showDecided) _settleCall(b);
    }
  }

  /// `_actorLabelFor` (ui.js:937-940) — the strings are the en table for now
  /// (`Assistant` / `You`); localized names land with the string tables.
  String _actorLabelFor(_Row b) {
    if (b.isAgent) {
      return (b.ref.isEmpty || b.ref == 'main') ? 'Assistant' : b.ref;
    }
    return b.ref.isEmpty ? 'You' : b.ref;
  }

  /// The ask-record state machine (ui.js:1067-1073, P-FOLD-038). The other
  /// half of ui.js's _askRecord — askRows of answered sets — is the sheet
  /// wizard's (Phase 5).
  HfyAskState _askRecord(_Row b, bool own) {
    if (b.showApproval) return own ? HfyAskState.pending : HfyAskState.other;
    if (b.decision == 'allowed') return HfyAskState.answered;
    if (b.decision == 'denied') {
      return b.declineReason == 'timeout'
          ? HfyAskState.timeout
          : HfyAskState.skipped;
    }
    if (b.stopped) return HfyAskState.stopped;
    return HfyAskState.asking; // undecided but not yet surfaced
  }
}

/// A truthy value as the JS string it would coerce into; falsy → [fallback]
/// (`d.state ‖ old`, `d.reason ‖ ''`). Wire payloads type these as strings;
/// anything else degrades, the fold never crashes.
String _wireText(Object? v, String fallback) {
  if (!hfyTruthy(v)) return fallback;
  if (v is String) return v;
  return '$v';
}

/// ui.js:893's `a.ref ‖ main` tail for actorKeys.
String _orFallback(Object? a, String fallback) =>
    a is String && a.isNotEmpty ? a : fallback;

/// ui.js:893's `a.ref ‖ a.uuid ‖ ''` chain for user actorKeys.
String _orFallback2(Object? a, String b) => a is String && a.isNotEmpty ? a : b;
