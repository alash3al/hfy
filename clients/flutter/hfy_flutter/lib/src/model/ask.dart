// hfy.ask_user question records (ui.js:206-250). Question normalization,
// answer validation and answer wording — the sheet (Phase 5) consumes these
// through the fold's tool blocks. Pure Dart, verbatim port of the JS rules,
// with ui.js line refs on the non-obvious ones.

import 'dart:convert' show jsonDecode;

/// The ask tool's name (ui.js:206 HFY_ASK_TOOL): a call block with this name
/// is a question set, never a tool row (P-FOLD-010).
const String kHfyAskTool = 'hfy.ask_user';

/// Question kinds the protocol allows (ui.js:206 HFY_ASK_TYPES) and the
/// codepoint cap on typed text answers (ui.js:206 HFY_TEXT_MAX).
const List<String> kHfyAskTypes = <String>[
  'single',
  'multi',
  'text',
  'confirm',
];
const int kHfyAskTextMax = 2000;

/// The normalized ask states (`askRecord`, ui.js:1066-1082, P-FOLD-038).
/// Only `pending` rows show an Open button.
enum HfyAskState {
  pending, // own, open — the sheet covers it
  other, // someone else's open ask ("waiting for {name}")
  answered, // decision allowed
  skipped, // denied, reason not `timeout`
  timeout, // denied with declineReason `timeout`
  stopped, // settled with the turn's end, never awaiting a decision
  asking, // undecided but not yet surfaced (no approval yet)
}

String hfyAskStateName(HfyAskState s) =>
    s.name; // the ui.js string ('pending'… — same spelling)

/// One option of a single/multi question. value is the protocol id; label the
/// display wording (falls back to the value).
class HfyAskOption {
  final String value;
  final String label;

  const HfyAskOption({required this.value, required this.label});

  @override
  bool operator ==(Object other) =>
      other is HfyAskOption && other.value == value && other.label == label;

  @override
  int get hashCode => Object.hash(value, label);
}

/// A normalized question (ui.js:213-219): exactly the fields every consumer
/// reads — `required` defaults to true, the placeholder exists only for text
/// questions, options only for single/multi.
class HfyAskQuestion {
  final String id;
  final String text;
  final String type; // one of kHfyAskTypes
  final bool required;
  final String placeholder;
  final List<HfyAskOption> options;

  const HfyAskQuestion({
    required this.id,
    required this.text,
    required this.type,
    required this.required,
    required this.placeholder,
    required this.options,
  });

  /// `hfyAnswerOk` for this question and [value] (ui.js:223-230, P-FOLD-052):
  /// empty is allowed only for optional questions; single must be a known
  /// option value; multi must be known UNIQUE values in option order; text is
  /// capped at [kHfyAskTextMax] codepoints (rune iteration = JS Array.from);
  /// confirm is strictly boolean.
  bool answerOk(Object? value) => _answerOk(
    type: type,
    id: id,
    required: required,
    options: options,
    value: value,
  );
}

// A truthiness check kept local so this module stays dependency-free:
// any Map/List is truthy ('' and 0 are not), per lib/src/model/values.dart.
bool _isTruthy(Object? v) {
  if (v == null) return false;
  if (v is bool) return v;
  if (v is String) return v.isNotEmpty;
  if (v is num) return v != 0;
  return true;
}

String _asText(Object? v) {
  if (v == null) return '';
  if (v is String) return v;
  return '$v';
}

Object? _or(Object? a, Object? b) => _isTruthy(a) ? a : b;

/// `hfyAnswerEmpty` (ui.js:221): absent, null, all-whitespace text, or an
/// empty array.
bool _answerEmpty(Object? v) {
  if (v == null) return true;
  if (v is String) return v.trim().isEmpty;
  if (v is List) return v.isEmpty;
  return false;
}

bool _answerOk({
  required String type,
  required String id,
  required bool required,
  required List<HfyAskOption> options,
  required Object? value,
}) {
  // Enough to move on: optional may be empty; otherwise the value must fit
  // the type (ui.js:222-231).
  if (_answerEmpty(value)) return !required;
  final values = [for (final o in options) o.value];
  switch (type) {
    case 'single':
      return value is String && values.contains(value);
    case 'multi':
      if (value is! List) return false;
      // v.every((x, i) => vals.indexOf(x) !== -1 && v.indexOf(x) === i):
      // every entry must be a known value, and first-seen (no duplicates).
      for (var i = 0; i < value.length; i++) {
        final x = value[i];
        if (!values.contains(x) || value.indexOf(x) != i) return false;
      }
      return true;
    case 'text':
      // Cap is in codepoints, not utf16 units (ui.js:228 uses Array.from).
      return value is String && value.runes.length <= kHfyAskTextMax;
    case 'confirm':
      return value is bool;
  }
  return false;
}

/// `hfyAskQuestions` on the tool call's raw `arguments` (ui.js:208-220,
/// P-FOLD-051). `_raw` may also arrive as a JSON string; entries with a
/// missing/empty id or an invalid type are dropped; option values must be
/// non-null, and both value/label are stringified.
List<HfyAskQuestion> hfyAskQuestions(Object? raw) {
  final v = hfyAskParseBody(raw);
  final qs = v is Map<String, Object?> && v['questions'] is List
      ? v['questions'] as List<Object?>
      : const <Object?>[];
  final out = <HfyAskQuestion>[];
  for (final q in qs) {
    if (q is! Map<String, Object?>) continue; // JS `q &&`: only objects survive
    final id = q['id'];
    final type = q['type'];
    if (id is! String || id.isEmpty) {
      continue; // typeof q.id === 'string' && q.id
    }
    if (!kHfyAskTypes.contains(type)) continue;
    final choice = type == 'single' || type == 'multi';
    final rawOptions = q['options'];
    final opts = <HfyAskOption>[];
    if (choice && rawOptions is List) {
      // ui.js: options with a null value are dropped; value/label stringified
      for (final o in rawOptions) {
        if (o is! Map<String, Object?>) continue; // `o &&`
        if (o['value'] == null) continue; // `o.value != null`
        opts.add(
          HfyAskOption(
            value: _asText(o['value']),
            label: _asText(_or(o['label'], o['value'])),
          ),
        );
      }
    }
    out.add(
      HfyAskQuestion(
        id: id,
        text: _asText(_or(q['text'], id)),
        type: type as String,
        // `q.required !== false`: absent/null counts as required (ui.js:215).
        required: !(identical(q['required'], false)),
        placeholder: type == 'text' && _isTruthy(q['placeholder'])
            ? _asText(q['placeholder'])
            : '',
        options: opts,
      ),
    );
  }
  return out;
}

/// The `arguments` body of an ask call: ui.js accepts a JSON string (parsed,
/// [] on failure) or the object itself.
Object? hfyAskParseBody(Object? raw) {
  if (raw is String) {
    try {
      return jsonDecode(raw);
    } on FormatException {
      return const <String, dynamic>{};
    }
  }
  return raw;
}

/// `hfyAskComplete` (ui.js:232): every question answered validly enough to
/// move on.
bool hfyAskComplete(List<HfyAskQuestion> questions, Object? answers) {
  for (final q in questions) {
    if (!q.answerOk(answers is Map ? answers[q.id] : null)) return false;
  }
  return true;
}

/// `hfyAskPayload`: the `answers` body for client.approve — empty answers are
/// left out (optional may be omitted), strings are trimmed, arrays copied
/// (ui.js:234-242).
Map<String, Object?> hfyAskPayload(
  List<HfyAskQuestion> questions,
  Object? answers,
) {
  final src = answers is Map ? answers : null;
  final out = <String, Object?>{};
  for (final q in questions) {
    var v = src?[q.id];
    if (_answerEmpty(v)) continue;
    if (v is List) {
      out[q.id] = List<Object?>.of(v, growable: false);
    } else if (v is String) {
      out[q.id] = v
          .trim(); // ui.js:239 trims string answers, keeps arrays as-is
    } else {
      out[q.id] = v;
    }
  }
  return out;
}

/// Wording of one answer as words (ui.js:244-250, P-FOLD-053): an empty
/// answer renders [HfyAnswerLabels.none], booleans map to yes/no, multi
/// arrays comma-join the option labels, single answers show the label and
/// text answers show the raw string.
String hfyAnswerText(HfyAskQuestion q, Object? value, HfyAnswerLabels labels) {
  if (_answerEmpty(value)) return labels.none;
  String labelOf(Object? v) {
    final found = q.options.where((o) => o.value == _asText(v));
    return found.isEmpty ? _asText(v) : found.first.label;
  }

  if (value is bool) return value ? labels.yes : labels.no;
  if (value is List) return [for (final v in value) labelOf(v)].join(', ');
  return q.type == 'single' ? labelOf(value) : _asText(value);
}

/// The yes/no/none wordings `hfyAnswerText` and the review step read;
/// callers pass the localized strings (ui.js:1067/1511 pass t(…)).
class HfyAnswerLabels {
  final String yes;
  final String no;
  final String none;

  const HfyAnswerLabels({
    required this.yes,
    required this.no,
    required this.none,
  });
}
