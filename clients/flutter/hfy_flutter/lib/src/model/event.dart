// hfy event model — the wire envelope of docs/reference.md "Events" and the
// embedded web client (internal/httpapi/embed/client.js + ui.js). One
// envelope for the stream and for history:
//
//   {id, type, parent, actor:{uuid,type,ref}, v, data, created_at}
//
// `data` is an object (or, tolerated like ui.js hfyData, a JSON string parsed
// to one). `created_at` is real RFC3339 in the fixtures; parsing stays
// tolerant to null because the JS fold falls back to Date.now() anyway
// (ui.js:894). Unknown event types are kept, never dropped: the client
// listens to exactly the 14 names below (P-CLIENT-001) but a rolling deploy
// may serve newer shapes (docs/reference.md), and client.js dedupes/cursors
// by envelope id regardless of type (P-CLIENT-015).
import 'dart:convert' show jsonDecode, jsonEncode;

/// The SSE event names the client listens to (client.js:5-9, PARITY.md
/// P-CLIENT-001). Unnamed frames arrive too and the envelope dedupe absorbs
/// their possible double delivery.
// ignore: constant_identifier_names — mirrors client.js's EVENT_TYPES name.
const List<String> EVENT_TYPES = <String>[
  'turn',
  'turn.state',
  'turn.cancel_requested',
  'message.text',
  'reasoning',
  'tool_call',
  'tool_call.interrupted',
  'tool_result',
  'approval.requested',
  'approval.decided',
  'model.usage',
  'summary',
  'text.delta',
  'reasoning.delta',
];

/// A typed parse failure for a malformed envelope. Never an untyped crash:
/// any structural violation reports itself as this exception; tolerated
/// shapes (see [HfyEvent.fromJson]) degrade, not throw.
class HfyProtocolException implements Exception {
  final String message;

  const HfyProtocolException(this.message);

  @override
  String toString() => 'HfyProtocolException: $message';
}

/// `{uuid, type, ref}` — the shape of event `actor` (ui.js:941 and the wire).
class HfyActor {
  final String? uuid;
  final String? type;
  final String? ref;

  const HfyActor({this.uuid, this.type, this.ref});

  factory HfyActor.fromJson(Object? json) {
    if (json == null) return const HfyActor();
    if (json is! Map) {
      throw const HfyProtocolException('actor must be an object');
    }
    return HfyActor(
      uuid: optString(json['uuid'], 'actor.uuid'),
      type: optString(json['type'], 'actor.type'),
      ref: optString(json['ref'], 'actor.ref'),
    );
  }

  static String? optString(Object? value, String what) {
    if (value == null) return null;
    if (value is String) return value;
    throw HfyProtocolException('$what must be a string or null');
  }

  @override
  bool operator ==(Object other) =>
      other is HfyActor &&
      other.uuid == uuid &&
      other.type == type &&
      other.ref == ref;

  @override
  int get hashCode => Object.hash(uuid, type, ref);

  @override
  String toString() => 'HfyActor(uuid: $uuid, type: $type, ref: $ref)';
}

/// One wire envelope. Immutable value object: `data` is kept as decoded
/// (never re-encoded on the fly), `json` is the original envelope map for
/// pass-through consumers. Both are deep-frozen at parse time ([fromJson]),
/// so a caller mutating its source maps after the fact can never shift the
/// event's contents — or its structural equality (which dedupe Orleans-style
/// seen-sets rely on across reconnects).
class HfyEvent {
  final String? id;
  final String type;
  final String? parent;
  final HfyActor? actor;
  final int? v;
  final Map<String, dynamic> data;
  final DateTime? createdAt;

  /// The raw envelope as received, deep-frozen (unmodifiable, recursively
  /// copied once at parse time). Read anything from it; mutation throws.
  final Map<String, dynamic> json;

  const HfyEvent({
    required this.type,
    this.id,
    this.parent,
    this.actor,
    this.v,
    this.data = const {},
    this.createdAt,
    this.json = const {},
  });

  /// Parses a decoded SSE/history envelope. Structural violations throw
  /// [HfyProtocolException]; tolerated shapes degrade per the rules below.
  ///
  /// - root must be a JSON object
  /// - `type` is required, non-empty string
  /// - `id`/`parent` optional strings; `v` optional int
  /// - `actor` optional {uuid,type,ref} of strings
  /// - `data` follows ui.js hfyData (ui.js:113-117): absent/null → {},
  ///   JSON string → parsed ({} on failure), object → used; a raw array or
  ///   a JSON string holding one degrades to {} like any other non-object
  ///   (one bad envelope must not kill the stream); anything else
  ///   (numbers/bools) throws typed
  /// - `created_at` RFC3339 → UTC [DateTime]; absent or unparsable → null
  ///   (the fold falls back to now upstream, ui.js:894)
  factory HfyEvent.fromJson(Object? json) {
    if (json is! Map) {
      throw const HfyProtocolException('event envelope must be a JSON object');
    }
    final typeValue = json['type'];
    if (typeValue is! String || typeValue.isEmpty) {
      throw const HfyProtocolException('event envelope type is required');
    }
    final v = json['v'];
    int? version;
    if (v != null) {
      if (v is int) {
        version = v;
      } else if (v is num && v.isFinite && v == v.truncate()) {
        version = v.toInt();
      } else {
        throw const HfyProtocolException('event envelope v must be an int');
      }
    }
    final createdAtRaw = json['created_at'];
    return HfyEvent(
      id: HfyActor.optString(json['id'], 'event envelope id'),
      type: typeValue,
      parent: HfyActor.optString(json['parent'], 'event envelope parent'),
      actor: json['actor'] == null ? null : HfyActor.fromJson(json['actor']),
      v: version,
      data: _data(json['data']),
      createdAt: createdAtRaw is String
          ? DateTime.tryParse(createdAtRaw)
          : null,
      json: _unmodifiable(json),
    );
  }

  /// The envelope as an unmodifiable deep copy: mutating the SOURCE map
  /// (or anything nested in it) after parsing must not be able to shift
  /// this event's contents or equality ("$key" is a no-op for the String
  /// keys real JSON has, and keeps exotic hand-built object-keyed maps
  /// object-typed instead of crashing).
  static Map<String, dynamic> _unmodifiable(Map<dynamic, dynamic> source) =>
      Map<String, Object?>.unmodifiable(<Object?, Object?>{
        for (final Object? key in source.keys) '$key': _freeze(source[key]),
      });

  /// Deep-frozen copy of [value]: every nested Map/List becomes its own
  /// unmodifiable instance; String/num/bool/null are immutable already.
  static Object? _freeze(Object? value) {
    if (value is List) {
      return List<Object?>.unmodifiable(<Object?>[
        for (final Object? item in value) _freeze(item),
      ]);
    }
    if (value is Map) {
      return Map<String, Object?>.unmodifiable(<Object?, Object?>{
        for (final Object? key in value.keys) '$key': _freeze(value[key]),
      });
    }
    return value;
  }

  // ui.js hfyData parity (ui.js:113-117): absent/null → {}, a JSON string
  // parsed with {} on failure, an object used as-is. Arrays — raw, or
  // hidden inside a JSON string — and any other non-object DEGRADE to {}
  // (ui.js hfyData would return the array itself): one bad envelope must
  // never kill a whole stream, and this model's `data` is always an object.
  static Map<String, dynamic> _data(Object? x) {
    if (x == null) return const {};
    final Object? decoded;
    if (x is Map || x is List) {
      decoded = x;
    } else if (x is String) {
      decoded = tryJson(x);
    } else {
      throw const HfyProtocolException(
        'event envelope data must be an object or a JSON string',
      );
    }
    if (decoded is Map) {
      return Map<String, Object?>.unmodifiable(<Object?, Object?>{
        for (final Object? key in decoded.keys) '$key': _freeze(decoded[key]),
      });
    }
    return const {}; // a JSON string holding an array → {} (hfyData parity)
  }

  static Object? tryJson(String source) {
    try {
      return jsonDecode(source);
    } on FormatException {
      return null;
    }
  }

  bool get isKnownType => EVENT_TYPES.contains(type);

  /// Canonical rendering of [value]: key order does not leak into the key,
  /// so semantic equality never depends on JSON object key order.
  static String _canonical(Object? value) {
    if (value is Map) {
      final keys = value.keys.toList()..sort((a, b) => '$a'.compareTo('$b'));
      return '{${[for (final key in keys) '${jsonEncode(key)}:${_canonical(value[key])}'].join(',')}}';
    }
    if (value is List) {
      return '[${[for (final item in value) _canonical(item)].join(',')}]';
    }
    if (value is String) return jsonEncode(value);
    if (value is num || value is bool || value == null) return '$value';
    return jsonEncode('$value');
  }

  // Each event's canonical form and hash are computed ONCE, cached weakly
  // on the instance, no matter how often ==/hashCode run (dedupe/cursor
  // loops hit them on every reconnect). An Expando keeps every field final,
  // so the public const constructor stays const-constructible.
  static final Expando<String> _canonicalCache = Expando<String>();
  static final Expando<int> _hashCodeCache = Expando<int>();

  String get _canonicalForm {
    final cached = _canonicalCache[this];
    if (cached != null) return cached;
    return _canonicalCache[this] = _canonical(data);
  }

  @override
  bool operator ==(Object other) =>
      other is HfyEvent &&
      other.id == id &&
      other.type == type &&
      other.parent == parent &&
      other.actor == actor &&
      other.v == v &&
      other.createdAt?.microsecondsSinceEpoch ==
          createdAt?.microsecondsSinceEpoch &&
      other._canonicalForm == _canonicalForm;

  @override
  int get hashCode {
    final cached = _hashCodeCache[this];
    if (cached != null) return cached;
    return _hashCodeCache[this] = Object.hash(
      id,
      type,
      parent,
      actor,
      v,
      createdAt?.microsecondsSinceEpoch,
      _canonicalForm.hashCode,
    );
  }

  @override
  String toString() => 'HfyEvent(id: $id, type: $type, parent: $parent, v: $v)';
}
