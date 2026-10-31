// Token grant + the tolerant expiry parsing of client.js (client.js:12-25,
// PARITY.md P-CLIENT-002/P-CLIENT-017). The onTokenNeeded callback must
// return a token string or {token, expires_at?}; the same validation
// applies in Dart via [HfyGrant.fromResult].
//
// DEVIATIONS from client.js parseExpiryMs (for the PARITY.md deviation
// list — copy verbatim when porting):
//
//   1. `''` / `' '` (a string Date.parse rejects AND `Number` turns into
//      0): JS → 0 ms, an already-expired instant; Dart → null (no
//      proactive refresh). ACCEPTED: an expiry that parses to nothing
//      cannot be trusted, and scheduling refresh at epoch zero would
//      hammer the server with a refresh storm.
//   2. Expiries beyond the JS-Date range (|ms| > 8.64e15 — e.g. 1e16,
//      9e18, '99999999999999999'): JS keeps the raw number (a harmlessly
//      far future); Dart → null, because no [DateTime] can physically
//      hold that instant (construction past ±8.64e15 ms fails), and "no
//      expiry" is the honest downgrade for an unusable timestamp.
//   3. Digit-only strings: both sides land in the epoch-number branch
//      (JS Date.parse returns NaN for epoch-sized digits, Dart cannot
//      parse digits as dates at all). JS ADDITIONALLY reads four-digit
//      strings as years ('2026' → year 2026); Dart reads '2026' as 2026
//      epoch seconds. Real grants carry RFC3339 or epoch numbers, so no
//      real bearer is affected.
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
  /// string token, or `{token, expires_at?}` (camelCase tolerated). The
  /// token must BE a string (the web client's own `typeof` guard) but any
  /// missing/empty/non-string case raises the SAME hard error the web
  /// client throws (client.js:178, 'onTokenNeeded returned no token').
  factory HfyGrant.fromResult(Object? result) {
    final Object? token = result is String
        ? result
        : result is Map
        ? result['token']
        : null;
    if (token is! String || token.isEmpty) {
      throw const HfyProtocolException('onTokenNeeded returned no token');
    }
    final ms = parseExpiryMs(result);
    return HfyGrant(
      token: token,
      expiresAt: ms == null
          ? null
          : DateTime.fromMillisecondsSinceEpoch(ms, isUtc: true),
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
      return _boundedMs(value.millisecondsSinceEpoch);
    }
    if (value is num) {
      if (!value.isFinite) return null;
      // Bounded in double space BEFORE any int conversion: out-of-range
      // numbers must never reach .round() (int64 clamping on the VM,
      // honest errors elsewhere).
      final ms = (value < 1e12 ? value * 1000 : value).toDouble();
      if (ms.abs() > _maxDateMs) return null;
      return ms.round();
    }
    if (value is String) {
      // Parity note (client.js:19-22): JS runs Date.parse FIRST, which
      // returns NaN for epoch-sized digit strings (→ its numeric branch)
      // but does read four-digit strings as YEARS ('2026'). Dart cannot
      // parse digit strings as dates at all, so digits go straight to the
      // numeric branch: the same result for every bearing a real grant
      // uses — see the deviation list at the top of this file.
      if (_digitsOnly.hasMatch(value)) {
        final numeric = num.tryParse(value);
        if (numeric != null) return parseExpiryValue(numeric);
      }
      final parsed = DateTime.tryParse(value);
      if (parsed != null) {
        return _boundedMs(parsed.millisecondsSinceEpoch);
      }
      final numeric = num.tryParse(value); // e.g. scientific notation
      if (numeric != null) return parseExpiryValue(numeric);
    }
    return null;
  }

  /// The representable Date range (ECMAScript): |ms| ≤ 8.64e15, the bound
  /// JS Date.max and Dart's DateTime share — construction past it is an
  /// error, so an unusable expiry is refused as null, never a crash.
  static const int _maxDateMs = 8640000000000000;

  static int? _boundedMs(int ms) => ms.abs() > _maxDateMs ? null : ms;

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
