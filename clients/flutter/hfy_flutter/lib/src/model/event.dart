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
// listens to the 13 durable + 2 ephemeral names below but a rolling deploy
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
/// pass-through consumers.
class HfyEvent {
  final String? id;
  final String type;
  final String? parent;
  final HfyActor? actor;
  final int? v;
  final Map<String, dynamic> data;
  final DateTime? createdAt;

  /// The raw envelope as received. Treat as read-only; never mutate.
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
  ///   JSON string → parsed ({} on failure), object → used; anything else
  ///   (arrays/numbers the server never sends) throws typed
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
      createdAt:
          createdAtRaw is String ? DateTime.tryParse(createdAtRaw) : null,
      json: Map<String, dynamic>.from(json),
    );
  }

  // ui.js hfyData parity (ui.js:113-117): absent/null → {}, a JSON string
  // parsed with {} on failure, an object used as-is. A string decoding to a
  // non-object degenerates to {} (hfyData would return the array — the
  // server never sends those; the typed model keeps {}).
  static Map<String, dynamic> _data(Object? x) {
    if (x == null) return const {};
    if (x is Map<String, dynamic>) return x;
    if (x is Map) return Map<String, dynamic>.from(x);
    if (x is String) {
      final decoded = tryJson(x);
      if (decoded is Map) return Map<String, dynamic>.from(decoded);
      return const {};
    }
    throw const HfyProtocolException(
        'event envelope data must be an object or a JSON string');
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
      _canonical(other.data) == _canonical(data);

  @override
  int get hashCode => Object.hash(
        id,
        type,
        parent,
        actor,
        v,
        createdAt?.microsecondsSinceEpoch,
        _canonical(data).hashCode,
      );

  @override
  String toString() => 'HfyEvent(id: $id, type: $type, parent: $parent, v: $v)';
}
