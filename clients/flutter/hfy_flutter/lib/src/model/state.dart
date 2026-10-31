// The fold's data model — read shapes + the pure helpers the fold needs that
// have no Flutter input: viewer identity, tool-call modes, calendar-day keys,
// the safe-url rule and the attachment mapping. The fold algorithm lives in
// fold.dart, ask records in ask.dart, card records in card.dart.
//
// No flutter imports, no dart:io: the fold runs identically under `flutter
// test` and pure `dart` (Phase 1 rule).

import 'ask.dart' show HfyAskQuestion, HfyAskState;
import 'values.dart' show hfyFirst, hfyOr, hfyString;

/// Terminal turn states (ui.js:34 HFY_TERMINAL). A turn that ends takes its
/// as-yet-undecided calls with it (P-FOLD-016) and turns every streaming
/// block off (P-FOLD-023).
const List<String> kHfyTerminalStates = <String>[
  'completed',
  'failed',
  'stopped',
  'cancelled',
];

/// Active turn states (ui.js:36 HFY_ACTIVE): they keep the composer in stop
/// mode and the typing dots on the first of them (P-FOLD-022/036).
const List<String> kHfyActiveStates = <String>[
  'queued',
  'running',
  'awaiting_approval',
];

/// Tool-call visibility modes (ui.js:35 HFY_TOOL_MODES, P-CFG-015). Anything
/// outside the three names means `summary` (P-CFG-003).
enum HfyToolCalls { hidden, summary, details }

/// The grant's actor — the viewer identity the fold reads once per history
/// page (client.js #viewer). Turn/block own-ness compares [uuid] (P-FOLD-021).
class HfyViewer {
  final String? uuid;
  final String? type;
  final String? ref;

  const HfyViewer({this.uuid, this.type, this.ref});

  factory HfyViewer.fromJson(Object? json) {
    if (json is Map) {
      return HfyViewer(
        uuid: json['uuid'] is String ? json['uuid'] as String : null,
        type: json['type'] is String ? json['type'] as String : null,
        ref: json['ref'] is String ? json['ref'] as String : null,
      );
    }
    return const HfyViewer();
  }
}

/// Wall-clock injection for the `Date.now()` fallback of an unparsable
/// created_at (ui.js:891) — tests inject a fixed instant to stay
/// deterministic; the real client uses the system clock.
abstract class HfyClock {
  DateTime now();
}

class HfySystemClock implements HfyClock {
  const HfySystemClock();

  @override
  DateTime now() => DateTime.now().toUtc();
}

/// Calendar-day keys ("en-CA" YYYY-MM-DD, ui.js:1052-1054) in the device time
/// zone. The fixture harness pins the choice to UTC (clients/fixtures/
/// README.md, the TZ=UTC note), so the default zone is UTC; a named zone
/// arrives with the UI phase, which carries the platform tz database.
class HfyTz {
  final String id;
  final Duration offset;

  const HfyTz.utc() : this.fixed('UTC', Duration.zero);

  const HfyTz.fixed(this.id, this.offset);

  String dayKey(int msSinceEpoch) {
    final d = DateTime.fromMillisecondsSinceEpoch(
      msSinceEpoch + offset.inMilliseconds,
      isUtc: true,
    );
    final mm = d.month.toString().padLeft(2, '0');
    final dd = d.day.toString().padLeft(2, '0');
    return '${d.year.toString().padLeft(4, '0')}-$mm-$dd';
  }

  @override
  String toString() => 'HfyTz($id, +${offset.inMinutes}min)';
}

/// `hfySafeUrl` (ui.js:308-311, P-FOLD-020): an attachment url is
/// server-given (signed, possibly same-origin relative); RELATIVE urls
/// resolve against [base] (the embed resolves against the page location —
/// Dart passes the configured API base) and only http/https survive;
/// `javascript:` and other unsafe schemes drop to `''`.
String hfySafeUrl(Object? raw, {Uri? base}) {
  if (raw is! String || raw.isEmpty) return '';
  final Uri u;
  try {
    u = base == null ? Uri.parse(raw) : base.resolve(raw);
  } on FormatException {
    return '';
  }
  final scheme = u.scheme.toLowerCase();
  return scheme == 'http' || scheme == 'https' ? u.toString() : '';
}

/// One mapped attachment entry (`mapAtts`, ui.js:928-936): exactly the fields
/// the template reads. The internal `k` row key (a stable x-for :key,
/// ui.js:933) is deliberately absent — see clients/fixtures/README.md's
/// text-block attachments row.
class HfyAttachment {
  final String url; // absolute after hfySafeUrl; '' when unsafe/plain
  final String label; // `name ‖ mime ‖ t('file')`
  final bool isImage; // safe url + image/* mime → renders in `images`
  final bool isLink; // safe url, not an image → a downloadable link row
  final bool isPlain; // no safe url → not clickable

  const HfyAttachment({
    required this.url,
    required this.label,
    required this.isImage,
    required this.isLink,
    required this.isPlain,
  });

  @override
  bool operator ==(Object other) =>
      other is HfyAttachment &&
      other.url == url &&
      other.label == label &&
      other.isImage == isImage &&
      other.isLink == isLink &&
      other.isPlain == isPlain;

  @override
  int get hashCode => Object.hash(url, label, isImage, isLink, isPlain);

  @override
  String toString() => 'HfyAttachment($label, $url)';
}

/// `mapAtts` (ui.js:928-936): every entry of a message's attachments array,
/// urls resolved by [hfySafeUrl]. [fileLabel] stands in for the localized
/// t('file') label (`File` in the en strings table, ui.js:14; custom strings
/// arrive with the controller in a later phase).
List<HfyAttachment> hfyMapAttachments(
  Object? list, {
  Uri? base,
  String fileLabel = 'File',
}) {
  final out = <HfyAttachment>[];
  if (list is! List) return out; // Array.isArray(list) ? list : []
  for (final item in list) {
    final a = item is Map<String, Object?>
        ? item
        : const <String, Object?>{}; // a = a || {}
    final rawUrl = hfyOr(a['url'], '');
    final url = hfySafeUrl(rawUrl, base: base);
    final mime = hfyString(hfyOr(a['mime'], '')); // String(a.mime || '')
    final img = url.isNotEmpty && mime.toLowerCase().startsWith('image/');
    // label: `name ‖ mime ‖ t('file')` over the ALREADY-stringified mime
    // (ui.js:932-933: `String(a.name || mime || self.t('file'))`, '' is falsy).
    final label = hfyString(
      hfyFirst(a['name'], mime.isNotEmpty ? mime : null, fileLabel),
    );
    out.add(
      HfyAttachment(
        url: url,
        label: label,
        isImage: img,
        isLink: url.isNotEmpty && !img,
        isPlain: url.isEmpty,
      ),
    );
  }
  return out;
}

// Below: the read shapes of the folded state. The fold keeps its mutable
// internal records (turns and blocks mutate as events fold — that is what the
// JS fold does); these accessors expose them read-only so a caller can never
// break fold invariants. HfyTurn/HfyBlock are implemented by fold.dart's
// private records.

/// One conversation participant as the messages see it (ui.js baseBlock:
/// 889-906). Blocks keep every delayed field on one class, like ui.js, so a
/// late event can fill any field in place.
abstract class HfyBlock {
  /// The folding event's id, or the synthetic `"<turnId>:stream"` /
  /// `"<turnId>:rstream"` ids of streaming blocks (ui.js:986/992).
  String get id;

  /// `text`, `reasoning` or `tool` — the only block kinds the fold builds
  /// (ui.js:889-906).
  String get kind;

  bool get isText;
  bool get isReasoning;
  bool get isTool;
  bool get isAgent;
  bool get isUser;

  /// The actor's `ref` (`''` when the envelope carried none).
  String get ref;

  /// The actor's uuid, the own-ness comparator (ui.js:893).
  String get actorUuid;

  /// `a:<ref‖main>` for agents, `u:<ref‖uuid>` for users (ui.js:893) — the
  /// group/actor key day and head separation compare.
  String get actorKey;

  /// ISO timestamp of the folding event (`b.iso`): ordering/tag; the
  /// formatted text is dropped presentation-only (README "iso").
  String get iso;

  /// `created_at` in ms since epoch for the 5-minute gap rule
  /// (ui.js:1140), and the Date.now() fallback of ui.js:891.
  int get ts;

  /// Calendar-day key in the injected zone (P-L10N-005); day separators
  /// compare these.
  String get dayKey;

  // grouping, computed in relayout() against the previous VISIBLE block of
  // any kind (ui.js:1139-1142, P-FOLD-027)
  bool get showDay;
  bool get showHead;
  bool get hidden; // tool rows only: the projection's `is-hidden` flag

  bool get streaming;
  bool get own; // per-block own-ness vs the viewer (b.isOwn)

  /// JS `a || b || c`-processed value; a string in every real payload, kept
  /// verbatim for fold parity with malformed ones.
  Object? get text;

  // setAtts → mapAtts results (ui.js:907-936), split like ui.js does
  List<HfyAttachment> get images;
  List<HfyAttachment> get files;

  // tool fields (defaults on text/reasoning kinds — one block class, ui.js)
  String get callId;
  String get name;
  String get status; // running | done | error | interrupted
  bool get isRunning;
  Object? get args; // the wire's `arguments` (null when none)
  Object? get content; // the result text (d.content || '')
  Object? get structured; // the wire's tool_result structured data
  bool get isAsk; // name === 'hfy.ask_user'
  List<HfyAskQuestion> get questions;
  Object? get answers; // finalAnswers: the decided answers (ask fallback)
  HfyAskState? get askState; // only meaningful on question sets
  String get decision; // allowed | denied | ''
  String get declineReason;
  bool get stopped; // settled with the turn's end (P-FOLD-016)
  bool get showApproval; // an open approval pause
  bool get showDecided; // the pause has settled
  bool get submitted; // the approval sheet POSTed this one (ui.js:1110)
  Object? get choice; // the sheet's pending allowed|denied (ui.js:1627)
  bool get showSummary;
  bool get showDetails;
  bool get showPendingOwn; // viewer must act (→ sheet)
  bool get showPendingOther; // others act (→ read-only note)
  Object? get card; // the tool's hfy/ui block verbatim
  bool get cardButtonsLive; // P-FOLD-041 liveness rule
  bool get hasQuestions; // questions.length > 0
}

/// One turn of the folded conversation (ui.js:868-872 ensureTurn + fold).
abstract class HfyTurn {
  /// The `turn` event's id; children carry it as `parent` (README header
  /// rules).
  String get id;

  Object? get agent; // d.agent || turn.agent — usually ""
  String get state; // 'queued' until a turn.state folds
  bool
  get stateSeen; // the merge only trusts a state that an event set (P-FOLD-043)
  Object? get error; // turn.state {state:"failed", error}
  bool get own; // the starter's actor uuid == viewer.uuid (P-FOLD-021)
  bool get showTyping; // assistant-is-typing dots (P-FOLD-036)
  String get userLabel; // the starter user's label for waiting-texts
  List<HfyBlock> get blocks; // oldest first
}
