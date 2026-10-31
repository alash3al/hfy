// Token grant + the tolerant expiry parsing of client.js (client.js:12-25,
// PARITY.md P-CLIENT-002/P-CLIENT-017). The onTokenNeeded callback must
// return a token string or {token, expires_at?}; the same validation
// applies in Dart via [HfyGrant.fromResult].
import '../model/event.dart';

/// The grant's token and its optional expiry. `expiresAt` is UTC at
/// millisecond precision (Date.getTime semantics of client.js); an expiry
/// the parser cannot trust becomes null — meaning "no proactive refresh".
class HfyGrant {
  final String token;
  final DateTime? expiresAt;

  const HfyGrant({required this.token, this.expiresAt});

  /// The expiry as epoch milliseconds, or null (client.js #expiresAtMs).
  int? get expiresAtMs => expiresAt?.millisecondsSinceEpoch;

  /// Parses what `onTokenNeeded` returned (client.js:175-181): a plain
  /// string token, or `{token, expires_at?}` (camelCase tolerated). A
  /// missing/empty token is the same hard error message the web client
  /// throws ('onTokenNeeded returned no token').
  factory HfyGrant.fromResult(Object? result) {
    final token = result is String
        ? result
        : result is Map ? HfyActor.optString(result['token'], 'token') : null;
    if (token == null || token.isEmpty) {
      throw const HfyProtocolException('onTokenNeeded returned no token');
    }
    final ms = parseExpiryMs(result);
    return HfyGrant(
      token: token,
      expiresAt:
          ms == null ? null : DateTime.fromMillisecondsSinceEpoch(ms, isUtc: true),
    );
  }

  /// Number of milliseconds since epoch, epoch in seconds or a RFC3339.
  ///
  /// Exact port of client.js parseExpiryMs (client.js:12-25):
  /// - accepts `expires_at` first, then `expiresAt` (camelCase)
  /// - null/absent, or a result that is not an object → null
  /// - a number: `< 1e12` → epoch seconds (×1000); otherwise epoch ms
  /// - a string: parsed as a date (the wire format is RFC3339), else a
  ///   numeric string with the same seconds/ms rule
  /// - anything untrusted → null (no proactive refresh)
  static int? parseExpiryMs(Object? result) {
    if (result is! Map) return null;
    final value = result['expires_at'] ?? result['expiresAt'];
    return parseExpiryValue(value);
  }

  /// The [parseExpiryMs] rules applied to the raw expiry value itself.
  /// Dart has no `Date` object; a [DateTime] is the exact analog.
  static int? parseExpiryValue(Object? value) {
    if (value == null) return null;
    if (value is DateTime) {
      try {
        return value.millisecondsSinceEpoch;
      } on Exception {
        return null;
      }
    }
    if (value is num) {
      if (!value.isFinite) return null;
      final ms = (value < 1e12 ? value * 1000 : value).toDouble();
      // Out-of-representable doubles → null rather than garbage-across-int64.
      if (ms.abs() > 9.2e18) return null;
      return ms.round();
    }
    if (value is String) {
      // NOTE (parity): JS Date.parse refuses all-digit strings (→ the
      // numeric branch of client.js:21-22), while Dart's DateTime.parse
      // would happily read them as YEARs. Keep the JS behaviour: digits are
      // ALWAYS the epoch-seconds/ms branch.
      if (_digitsOnly.hasMatch(value)) {
        final numeric = num.tryParse(value);
        if (numeric != null) return parseExpiryValue(numeric);
      }
      final parsed = DateTime.tryParse(value);
      if (parsed != null) return parsed.millisecondsSinceEpoch;
      final numeric = num.tryParse(value); // e.g. scientific notation
      if (numeric != null) return parseExpiryValue(numeric);
    }
    return null;
  }

  static final RegExp _digitsOnly = RegExp(r'^[+-]?\d+$');

  @override
  bool operator ==(Object other) =>
      other is HfyGrant &&
      other.token == token &&
      other.expiresAt?.millisecondsSinceEpoch ==
          expiresAt?.millisecondsSinceEpoch;

  @override
  int get hashCode => Object.hash(token, expiresAt?.millisecondsSinceEpoch);

  @override
  String toString() => 'HfyGrant(token: ┄, expiresAt: $expiresAt)';
}
