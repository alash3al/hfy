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

  /// The fold's own block list; consumers read [blocks] — a fresh immutable
  /// copy rebuilt after every append, so nobody outside this library can
  /// mutate or alias it.
  final List<_Row> _blocks = <_Row>[];
  List<HfyBlock>? _blocksView;

  /// Oldest first (ui.js keeps them push/prepend ordered).
  @override
  List<HfyBlock> get blocks =>
      _blocksView ??= List.unmodifiable(_blocks);

  void addBlock(_Row b) {
    _blocks.add(b);
    _blocksView = null;
  }
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

  /// setAtts' split (ui.js:907-911). Internal lists — the fold is the only
  /// writer; public [images]/[files] hand out immutable copies.
  List<HfyAttachment> _images = const <HfyAttachment>[];
  List<HfyAttachment> _files = const <HfyAttachment>[];
  List<HfyAttachment>? _imagesView;
  List<HfyAttachment>? _filesView;

  @override
  List<HfyAttachment> get images =>
      _imagesView ??= List.unmodifiable(_images);

  @override
  List<HfyAttachment> get files =>
      _filesView ??= List.unmodifiable(_files);

  void setAtts(List<HfyAttachment> images, List<HfyAttachment> files) {
    _images = images;
    _files = files;
    _imagesView = null;
    _filesView = null;
  }

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
    this.tz = const HfyTz.local(),
    this.fileLabel = 'File',
  }) : clock = clock ?? const HfySystemClock();

  /// The grant's actor; own-ness compares its uuid (P-FOLD-021).
  final HfyViewer? viewer;

  /// The tool-call visibility mode (config.tool_calls, P-CFG-015).
  final HfyToolCalls toolCalls;

  /// Page-origin stand-in for `new URL(raw, location.href)` (ui.js:308-311):
  /// relative attachment urls resolve against it (P-FOLD-020).
  final Uri? base;

  final HfyClock clock;

  /// The zone day separators group in — [HfyTz.local] (the device's, DST
  /// aware) by default, like ui.js's Intl formatter; UTC only where injected:
  /// the fixture harnesses pin it so projections never depend on the machine
  /// (clients/fixtures/README.md, the TZ=UTC note, P-FOLD-027).
  final HfyTz tz;

  /// The `File` fallback of attachment labels (ui.js:933's t('file') with the
  /// en strings table; a caller fixes it per locale — set once, never the
  /// fold's business afterwards).
  final String fileLabel;

  final List<_Turn> _turns = <_Turn>[];

  /// [revision]-keyed view of [_turns]: reads between two state changes
  /// return the SAME list instance (no copy per read — the controller's
  /// build loops hit this getter hard), rebuilt when the revision moves.
  List<HfyTurn>? _turnsView;

  /// Folded turns, oldest first (ui.js keeps them push/prepend ordered;
  /// loadOlder puts older ones in front). Fresh immutable copy after every
  /// revision bump; the turns themselves stay live records — block identity
  /// never changes even when a block is refilled in place (durable text
  /// replacing its stream block).
  List<HfyTurn> get turns => _turnsView ??= List.unmodifiable(_turns);

  bool _loaded = false; // ui.js:856 (`this._loaded = true` after connect)
  bool _ownActive = false;
  bool _showWelcome = false;

  // approval sheet state (ui.js:1480-1536 syncSheet)
  List<_Row> _pendingOwn = const <_Row>[]; // rebuilt by relayout, unmodifiable
  bool _sheetOpen = false;
  bool _showActionPill = false;
  bool _sheetMin = false;
  final Set<String> _seenPending = <String>{};
  String?
  _awaitTurn; // ui.js:799: bridges send() and the turn's first user text

  /// One-up counter of folded state (monotonic, never resets): every handled
  /// [apply] bumps it — a handled event always moves the view's ground even
  /// when no row changed (an unknown call_id's decided still seats its turn)
  /// — every [applyAll] batch at most once, every [mergeOlderPage] when its
  /// merge moved anything. Setup calls ([markLoaded], the sheet/await
  /// bridges) do NOT — they move derived outlets, never turns/blocks.
  /// Watchers compare [revision] to notice conversation content; [turns]
  /// caches until it changes.
  int _revision = 0;
  int get revision => _revision;

  void _bumpRevision() {
    _revision++;
    _turnsView = null;
  }

  // ---- public derived state --------------------------------------------------

  /// `showWelcome` (ui.js:795, P-FOLD-046). NOTE it reads the COMPONENT's
  /// hasOlder — only a loadOlder round moves that, never the client's
  /// initial pointer.
  bool get showWelcome => _showWelcome;

  /// Stop-mode (ui.js:799-801, P-FOLD-022): the viewer's own turn is
  /// queued/running/awaiting_approval, or a send's turn id still bridges.
  bool get ownActive => _ownActive;

  /// The embed component's hasOlder mirror (ui.js:857/1241). Not a public
  /// mutable field: a connect() reports it through [setHasOlder], loadOlder
  /// through [mergeOlderPage].
  bool _hasOlder = false;
  bool get hasOlder => _hasOlder;

  /// ui.js connect's `this.hasOlder = hfyClient.hasOlder` (ui.js:861):
  /// assignment plus one [syncDerived], which is what the embed's caller
  /// does right after.
  void setHasOlder(bool value) {
    _hasOlder = value;
    syncDerived();
  }

  /// The viewer's open approvals in folded call order (ui.js:1110, P-FOLD-042)
  /// — question sets included; the sheet separates them by hasQuestions.
  List<String> get pendingOwn => [for (final b in _pendingOwn) b.callId];

  /// The same calls as blocks, in call order (the sheet's read side). Rebuilt
  /// only by relayout, so the same instance comes back until the fold changes.
  List<HfyBlock> get pendingOwnBlocks => _pendingOwn;

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

  /// The approval sheet POSTed [callId] (ui.js:1110's `b.submitted = true`):
  /// its blocks leave [pendingOwn] now, before the server's approval.decided
  /// folds. Idempotent: a call already marked, or an unknown id, changes
  /// nothing and bumps no revision.
  void markSubmitted(String callId) {
    var moved = false;
    for (final turn in _turns) {
      for (final b in turn._blocks) {
        if (b.isTool && !b.submitted && b.callId == callId) {
          b.submitted = true;
          moved = true;
        }
      }
    }
    if (!moved) return;
    _bumpRevision();
    relayout();
    syncDerived();
  }

  /// The send() bridge (ui.js:1287's `_awaitTurn`): show Stop until the turn
  /// owns a user text block (ui.js:799-800) — a fixture-driven wave never
  /// sets it; the controller phase does. ui.js derives right away (1287).
  void awaitTurn(String turnId) {
    _awaitTurn = turnId;
    syncDerived();
  }

  /// ui.js:1462's doStop clears the bridge at once (and syncs, 1463).
  void clearAwaitTurn() {
    _awaitTurn = null;
    syncDerived();
  }

  // ---- the fold (ui.js:958-1050) ----------------------------------------------

  /// One envelope through `fold(env, isHistory)` (ui.js:961-1049). Returns
  /// true when the event was HANDLED — the fold ran a type branch at all —
  /// and false only when the fold dropped it entirely (ui.js's `return` on
  /// unknown types, P-FOLD-057). A re-applied event whose row only has its
  /// settled values set again still returns true — replay/dup detection is
  /// the caller's (HfyDedupe); the fold never compares values.
  ///
  /// Every handled event relayouts and syncs like ui.js:1044-1046, changed
  /// rows or not: the fold's `_ensureTurn` can create the turn the event
  /// names (an approval.decided / tool_call.interrupted for a call_id the
  /// fold never saw), and the queued turn must seat NOW — visible in [turns],
  /// revision moved, typing dots per ui.js — never a fold that drifted apart
  /// from its record of turns.
  ///
  /// History events belong to [mergeOlderPage] (one layout per page — the
  /// only caller that ever needs a collector).
  bool apply(HfyEvent e, {bool history = false}) {
    final r = _foldInto(_turns, e);
    if (!r.hands) return false; // dropped/unknown type — no layout, no revision
    _bumpRevision();
    if (r.layout) relayout(); // ui.js:1044 — a handled event gets a relayout
    syncDerived();
    return true;
  }

  /// A WHOLE batch (a replay backlog) through `fold()` with ONE trailing
  /// [relayout] + [syncDerived] instead of one per event — the O(n²) of
  /// replaying n events each with a full layout drops to relayout's own O(n).
  /// Safe because relayout() is a full recompute from the folded blocks
  /// alone: grouping, typing dots, ask states and tool visibility depend on
  /// nothing but folds. The two enums are kept exact with two per-fold
  /// slices:
  ///
  ///   - the terminal turn's streaming flags are applied inline by
  ///     [_foldInto] (not collect mode), so batched folds see exactly what
  ///     event-at-a-time folding saw;
  ///   - [_sheetFoldCheck] runs the syncSheet-visible slice of the sheet
  ///     bookkeeping per fold (a pending call surfaced by this fold gets its
  ///     _seenPending mark and opens a minimized sheet), because _sheetMin/
  ///     _seenPending are ORDER-sensitive — the minimized-sheet reopen rule
  ///     must fire for a call requested and decided in the same batch, as it
  ///     does event-at-a-time (item 2 regression, fold_api_test.dart).
  /// Asserted identical to per-event [apply] — with minimize/open interleave
  /// points — for every fixture plus randomized orderings in
  /// test/fold/fold_api_test.dart. The [history] flag exists for call-site
  /// symmetry with [apply]; both fold the same list — pages arrive as data
  /// in [mergeOlderPage], not streamed through here. Returns whether ANY
  /// event changed state (revision bumped at most once per batch, once when
  /// any event was handled).
  bool applyAll(Iterable<HfyEvent> events, {bool history = false}) {
    var hands = false;
    var layout = false;
    var changed = false;
    for (final e in events) {
      final r = _foldInto(_turns, e);
      if (!r.hands) continue; // dropped/unknown — the batch never saw it
      hands = true;
      if (r.changed) changed = true;
      if (r.layout) layout = true;
      final t = r.turn;
      if (t != null) {
        _sheetFoldCheck(
          t,
          callRequested: e.type == 'approval.requested',
          userTextLanded: e.type == 'message.text' && e.actor?.type != 'agent',
        );
      }
    }
    if (hands) _bumpRevision();
    if (layout) relayout(); // a batch is laid out once, like a page
    if (hands) syncDerived();
    return changed;
  }

  /// loadOlder() in ONE call (ui.js:1220-1244): fold [page] into a fresh
  /// collector with NO layout, merge it into the turns (unseen block ids
  /// appended, state overwritten only by a collected stateSeen turn, older
  /// turns prepended), then one relayout + syncDerived and
  /// `_hasOlder = [hasMore]`.
  ///
  /// The collector is scoped to this call — ui.js keeps `_collecting` open
  /// across an await and nulls it in its catch (ui.js:1245); folding into a
  /// local list does that housekeeping structurally: if the fold throws
  /// mid-page the local list is simply dropped, nothing is merged, and
  /// `_hasOlder`/turns stand exactly as before. The page folds run WITHOUT
  /// the relayout-time kills (ui.js folds collection events and returns
  /// before relayouting, ui.js:1043) — a page's delta-stream block must stay
  /// alive until the merge's one relayout, so a page's later durable text
  /// replaces it in place instead of leaving a dead block behind.
  /// DEDUPE stays client-side — the caller applies HfyDedupe#claim before
  /// handing envelopes here.
  void mergeOlderPage(List<HfyEvent> page, {required bool hasMore}) {
    final collected = <_Turn>[];
    for (final e in page) {
      _foldInto(collected, e, collecting: true);
    }
    _mergeCollected(collected, hasMore: hasMore);
  }

  /// `loadOlder()`'s merge half (ui.js:1225-1243): match the collected turns
  /// against the live ones, append only unseen block ids, overwrite the state
  /// ONLY when the collected turn actually saw a turn.state event, settle
  /// anything the merge drags under a terminal state, prepend the brand-new
  /// turns, then one relayout + sync over the final list.
  void _mergeCollected(List<_Turn> collected, {required bool hasMore}) {
    final byId = <String, _Turn>{for (final t in _turns) t.id: t};
    final prepend = <_Turn>[];
    for (final ct in collected) {
      final existing = byId[ct.id];
      if (existing == null) {
        prepend.add(ct); // a turn the live page never carried
        continue;
      }
      final seenBlocks = <String>{for (final b in existing._blocks) b.id};
      for (final b in ct._blocks) {
        if (seenBlocks.add(b.id)) existing.addBlock(b);
      }
      // only a collected turn that actually saw a turn.state event may
      // overwrite state (ui.js:1234)
      if (ct.stateSeen && ct.state.isNotEmpty) existing.state = ct.state;
      if (hfyTruthy(ct.error)) existing.error = ct.error;
      // a merge can drag in untouched asks while restoring a terminal state:
      // they settle like any other end of turn, idempotently (ui.js:1238)
      if (kHfyTerminalStates.contains(existing.state)) {
        _settleCalls(existing);
      }
    }
    // a call is a state change when it merged anything or when hasOlder
    // actually moves (an empty page holding hasMore steady changes nothing)
    if (collected.isNotEmpty ||
        prepend.isNotEmpty ||
        _hasOlder != hasMore) {
      _bumpRevision();
    }
    _turns.insertAll(0, prepend);
    _hasOlder = hasMore; // ui.js:1241
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
      final starter = _starterOf(turn);
      turn.own = starter != null && _rowOwns(starter);
      turn.userLabel = starter == null ? '' : _actorLabelFor(starter);
      for (final b in turn._blocks) {
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
      if (turn._blocks.isNotEmpty) last = turn._blocks.last;
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
    if (!_sameRows(_pendingOwn, pending)) {
      _pendingOwn = List.unmodifiable(pending);
    }
  }

  /// Same rows in the same order: the sheet's list stays one instance until
  /// the fold changes (see [pendingOwnBlocks]).
  static bool _sameRows(List<_Row> a, List<_Row> b) {
    if (a.length != b.length) return false;
    for (var i = 0; i < a.length; i++) {
      if (!identical(a[i], b[i])) return false;
    }
    return true;
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
        (t) => t.id == wait && t._blocks.any((b) => b.isText && b.isUser),
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
      for (final b in turn._blocks) {
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

  /// applyAll's per-fold slice of syncSheet (ui.js:1481-1485). The sheet's
  /// fresh/open bookkeeping is order-sensitive (a minimized sheet reopens on
  /// a NEW pending call), so a batch folds it per event: only two fold kinds
  /// can surface a pending call at their step — an approval.requested (the
  /// call itself) and the user text that first makes a turn the viewer's (a
  /// request sitting on it). Every other kind leaves the pending set as the
  /// previous step produced it, which the batch's final relayout re-covers.
  /// Marking here keeps _seenPending/_sheetMin identical to event-at-a-time
  /// folding — a call requested and decided inside one batch reopens the
  /// minimized sheet exactly as if the events were applied one by one
  /// (item 2's regression, test/fold/fold_api_test.dart) — while the
  /// expensive relayout stays deferred to the batch's end.
  void _sheetFoldCheck(
    _Turn t, {
    required bool callRequested,
    required bool userTextLanded,
  }) {
    // own-ness is relayout-derived, but only THIS turn was folded: derive it
    // from the blocks right where the fold ran (ui.js:1095-1098, relayout's
    // own rule) — the field may lag a starter that just landed in this batch.
    final starter = _starterOf(t);
    final own = starter != null && _rowOwns(starter);
    if (userTextLanded) {
      if (own == t.own) return; // own-ness didn't move at this fold
      t.own = own; // keep the field honest for the folds to come
      if (!own) return;
    } else if (callRequested) {
      if (!own) return; // other people's requests never surface (ui.js:1104)
    } else {
      return;
    }
    for (final b in t._blocks) {
      // pendingOwn's make (ui.js:1104-1110): showApproval calls of the
      // viewer's turns, question sets included — ask rows gate the sheet too
      if (!b.isTool || !b.showApproval || b.submitted) continue;
      if (_seenPending.add(b.callId)) {
        _sheetMin = false; // a new request surfaces the sheet (ui.js:1485)
      }
    }
  }

  // ---- fold internals ---------------------------------------------------------

  /// The switch under ui.js:963-1044 into one target list. `hands` says the
  /// type is handled at all (ui.js:1043's default-return, zero for unknown
  /// leaves like summary/model.usage, P-FOLD-057), `layout` whether relayout
  /// must run (pure delta appends skip it, ui.js:985/991 — everything else,
  /// including a decided/interrupted whose call id matches nothing, keeps the
  /// default true, because ui.js still relayouts for those), `changed`
  /// whether the fold moved a row/block at all, and [turn] the turn the fold
  /// acted on (null for unknown types).
  ///
  /// [collecting] marks the loadOlder page collector (ui.js's `_collecting`
  /// list): there ui.js folds and returns BEFORE any relayout (ui.js:1043),
  /// so the RELAYOUT-time kills stay out — a collected terminal turn keeps
  /// its stream blocks alive until the merge's one layout, exactly where a
  /// later page event's durable text replaces them in place.
  ({bool hands, bool layout, bool changed, _Turn? turn}) _foldInto(
    List<_Turn> list,
    HfyEvent e, {
    bool collecting = false,
  }) {
    var layout = true;
    _Turn? turn;
    final d = e.data;
    // Turn ids: `env.id` for type `turn`, else `env.parent ‖ env.id` (ui.js:961).
    final turnId = e.type == 'turn'
        ? (e.id ?? '')
        : (hfyTruthy(e.parent) ? e.parent! : (e.id ?? ''));
    switch (e.type) {
      case 'turn':
        turn = _ensureTurn(list, turnId);
        turn.agent = hfyOr(d['agent'], turn.agent);
        break;
      case 'turn.state':
        turn = _ensureTurn(list, turnId);
        turn.stateSeen = true;
        turn.state = _wireText(hfyOr(d['state'], turn.state), turn.state);
        final err = d['error'];
        if (hfyTruthy(err)) turn.error = err;
        // a turn that ends takes its as-yet-undecided calls with it; ui.js
        // additionally switches its streaming blocks off at RELAYOUT level
        // (ui.js:1100, right after this fold) — moved into the fold so the
        // batched folds of applyAll match event-at-a-time folding (a dead
        // stream block must never be merged-into by a later durable text).
        // The loadOlder collector has NO per-event relayout, so there the
        // kill stays out and the page folds exactly as ui.js's collector did.
        if (kHfyTerminalStates.contains(turn.state)) {
          _settleCalls(turn);
          if (!collecting) {
            for (final b in turn._blocks) {
              if (b.streaming) b.streaming = false;
            }
          }
        }
        break;
      case 'message.text':
        turn = _ensureTurn(list, turnId);
        final isAgent = e.actor?.type == 'agent';
        final streaming = isAgent ? _findStreamingBlock(turn, 'text') : null;
        if (streaming != null) {
          // the durable text REPLACES the streaming block in place
          // (ui.js:975-979: text, attachments, id, ts/iso/dayKey restamped,
          // streaming off)
          final nb = _newTextBlock(e, true, d);
          streaming.text = nb.text;
          streaming.setAtts(nb._images, nb._files);
          streaming.streaming = false;
          streaming.id = nb.id;
          streaming.ts = nb.ts;
          streaming.iso = nb.iso;
          streaming.dayKey = nb.dayKey;
        } else {
          turn.addBlock(_newTextBlock(e, isAgent, d));
        }
        break;
      case 'text.delta':
        turn = _ensureTurn(list, turnId);
        final b = _findStreamingBlock(turn, 'text');
        if (b == null) {
          final fresh = _newTextBlock(e, true, const <String, Object?>{});
          fresh.id = '$turnId:stream'; // ui.js:986
          // ui.js relayouts right after this fold and switches a TERMINAL
          // turn's streaming flag off (ui.js:1100) — a fresh stream block on
          // an already-terminal turn is thus born dead (its text still
          // lands, its id is still the synthetic one). Inlined so the
          // batched folds of applyAll match event-at-a-time; the loadOlder
          // collector has no per-event relayout, so there ui.js's plain
          // `streaming = true` stands and a later page event can still
          // replace the block durably.
          fresh.streaming = collecting || !kHfyTerminalStates.contains(turn.state);
          fresh.text = hfyOr(d['text'], '');
          turn.addBlock(fresh);
        } else {
          layout = false; // pure delta append: no relayout (ui.js:985)
          b.text = '${hfyString(b.text)}${hfyString(hfyOr(d['text'], ''))}';
        }
        break;
      case 'reasoning.delta':
        turn = _ensureTurn(list, turnId);
        final b2 = _findStreamingBlock(turn, 'reasoning');
        if (b2 == null) {
          final fresh = _baseBlock(e, 'reasoning');
          fresh.id = '$turnId:rstream'; // ui.js:992
          // as in text.delta: born dead on a terminal turn (ui.js:1100);
          // collecting keeps ui.js's plain `streaming = true`.
          fresh.streaming = collecting || !kHfyTerminalStates.contains(turn.state);
          fresh.text = hfyOr(d['text'], '');
          turn.addBlock(fresh);
        } else {
          layout = false;
          b2.text = '${hfyString(b2.text)}${hfyString(hfyOr(d['text'], ''))}';
        }
        break;
      case 'reasoning':
        turn = _ensureTurn(list, turnId);
        final b3 = _findStreamingBlock(turn, 'reasoning');
        if (b3 != null) {
          // P-FOLD-058: the durable reasoning event replaces text/streaming/id
          // but NOT ts/iso/dayKey — ui.js:997 only, unlike message.text:977.
          // ui.js:997 assigns `b.id = env.id` even when the envelope carried
          // no id (b.id becomes undefined, and the projection drops the key);
          // an HfyBlock id is non-nullable in Dart, so an id-less durable
          // event KEEPS the block's prior id (the stream-synthetic one) here —
          // documented in test/fold/fold_api_test.dart ("id-less durable
          // reasoning keeps the prior id").
          b3.text = hfyOr(d['text'], '');
          b3.streaming = false;
          if (e.id != null) b3.id = e.id!;
        } else {
          final fresh = _baseBlock(e, 'reasoning');
          fresh.text = hfyOr(d['text'], '');
          turn.addBlock(fresh);
        }
        break;
      case 'tool_call':
        turn = _ensureTurn(list, turnId);
        turn.addBlock(_newToolBlock(e, d));
        break;
      case 'tool_result':
        turn = _ensureTurn(list, turnId);
        var row = _findTool(turn, d['call_id']);
        if (row == null) {
          // a tool_result for a call the fold never saw (ui.js:1006) — and a
          // result whose call_id is missing/null NEVER matches an orphan row
          // (`callId: undefined === ''` is false), so each misses-into its own
          // fresh row (a present-but-empty id does match `''`)
          row = _newToolBlock(e, {'call_id': d['call_id']});
          turn.addBlock(row);
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
        turn = _ensureTurn(list, turnId);
        final row2 = _findTool(turn, d['call_id']);
        if (row2 == null) {
          // ui.js's find-then-maybe-set: layout stays true here (only a found
          // streaming-block append falsifies it), and the turn just seated by
          // _ensureTurn still counts as the fold acting on the conversation.
          return (hands: true, layout: true, changed: false, turn: turn);
        }
        _setToolStatus(row2, 'interrupted');
        break;
      case 'approval.requested':
        turn = _ensureTurn(list, turnId);
        final row3 = _findTool(turn, d['call_id']);
        _Row row;
        if (row3 == null) {
          row = _newToolBlock(e, {'call_id': d['call_id']});
          turn.addBlock(row);
        } else {
          row = row3;
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
        turn = _ensureTurn(list, turnId);
        final row4 = _findTool(turn, d['call_id']);
        if (row4 == null) {
          // an unknown call id — but the turn was still ensured just now, so
          // like ui.js (which relayouts and syncs at fold's tail) the fold
          // acted: the queued turn seats in the view this very apply.
          return (hands: true, layout: true, changed: false, turn: turn);
        }
        row4.showApproval = false;
        row4.showDecided = true;
        row4.decision = hfyTruthy(d['approve']) ? 'allowed' : 'denied';
        final answered = d['answers'];
        if (hfyTruthy(answered) && (answered is Map || answered is List)) {
          row4.answers = answered;
        }
        row4.declineReason = _wireText(hfyOr(d['reason'], ''), '');
        break;
      default:
        // P-FOLD-057: summary/model.usage (and unknown types) drop here — no
        // block, no layout. Envelopes reach the app through the controller's
        // event stream, never through the fold.
        return (hands: false, layout: false, changed: false, turn: null);
    }
    return (hands: true, layout: layout, changed: true, turn: turn);
  }

  _Turn _ensureTurn(List<_Turn> list, String id) {
    for (final t in list) {
      if (t.id == id) return t;
    }
    final turn = _Turn(id);
    list.add(turn);
    return turn;
  }

  /// `_findTool` (ui.js:873-877) on the RAW wire call_id: a missing/null id
  /// never matches — JS compares `block.callId === callId` strictly, and
  /// `undefined === ''` is false, while `'' (present) === ''` matches the
  /// orphan row an earlier miss created. Non-string values compare through
  /// their ui.js `String()` form (wire ids are strings; the coercion only
  /// unifies the degenerate number case).
  _Row? _findTool(_Turn turn, Object? rawCallId) {
    if (rawCallId == null) return null;
    final want = _wireText(rawCallId, '');
    for (final b in turn._blocks) {
      if (b.isTool && b.callId == want) return b;
    }
    return null;
  }

  /// `streamingBlock` (ui.js:878-884) — scans backward.
  _Row? _findStreamingBlock(_Turn turn, String kind) {
    for (var i = turn._blocks.length - 1; i >= 0; i--) {
      final b = turn._blocks[i];
      if (b.kind == kind && b.streaming) return b;
    }
    return null;
  }

  /// The turn's first user text block (ui.js:1095) — relayout's and the
  /// sheet bookkeeping's own-ness starter.
  _Row? _starterOf(_Turn turn) {
    for (final b in turn._blocks) {
      if (b.isText && b.isUser) return b;
    }
    return null;
  }

  /// ui.js:1097: the starter's actor uuid is the viewer's.
  bool _rowOwns(_Row starter) =>
      viewer != null &&
      hfyTruthy(viewer!.uuid) &&
      starter.actorUuid == viewer!.uuid;

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
    // setAtts split (ui.js:907-911)
    b.setAtts(
      [for (final a in atts)
        if (a.isImage) a,
      ],
      [for (final a in atts)
        if (!a.isImage) a,
      ],
    );
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
    for (final b in turn._blocks) {
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
