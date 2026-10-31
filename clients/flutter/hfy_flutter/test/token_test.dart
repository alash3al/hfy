// Grant expiry tolerance — exact replica of client.js parseExpiryMs
// (client.js:12-25, PARITY.md P-CLIENT-002): expires_at/expiresAt, a Date
// (DateTime in Dart), epoch seconds (< 1e12 → ×1000) or ms, an RFC3339
// string, or a numeric string with the same rule; anything else → null.
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/hfy_flutter.dart';

void main() {
  group('P-CLIENT-002 HfyGrant.parseExpiryMs (parseExpiryMs parity)', () {
    final rfc3339Ms =
        DateTime.utc(2026, 1, 2, 10, 30).millisecondsSinceEpoch;

    Map<String, Object?> of(Object? expiresAt, {bool camel = false}) =>
        camel ? {'expiresAt': expiresAt} : {'expires_at': expiresAt};

    test('RFC3339 string', () {
      expect(HfyGrant.parseExpiryMs(of('2026-01-02T10:30:00Z')), rfc3339Ms);
      expect(
        HfyGrant.parseExpiryValue('2026-01-02T10:30:00Z'),
        rfc3339Ms,
      );
    });

    test('Date-like value (a DateTime instance — the Dart analog of Date)', () {
      expect(HfyGrant.parseExpiryValue(DateTime.utc(2026, 1, 2, 10, 30)), rfc3339Ms);
    });

    test('epoch seconds vs milliseconds (< 1e12 rule)', () {
      expect(HfyGrant.parseExpiryValue(1727952000), 1727952000000);
      expect(HfyGrant.parseExpiryValue(1727952000000), 1727952000000);
      // boundaries: below ×1000, at-and-above unchanged (client.js:17,22)
      expect(HfyGrant.parseExpiryValue(999999999999), 999999999999000);
      expect(HfyGrant.parseExpiryValue(1e12), 1000000000000);
    });

    test('numeric strings use the same seconds/ms rule', () {
      expect(HfyGrant.parseExpiryValue('1727952000'), 1727952000000);
      expect(HfyGrant.parseExpiryValue('1727952000000'), 1727952000000);
      expect(HfyGrant.parseExpiryMs(of('2033-01-01T00:00:00Z')), isNotNull);
    });

    test('camelCase expiresAt (client.js:14)', () {
      expect(HfyGrant.parseExpiryMs(of('2026-01-02T10:30:00Z', camel: true)), rfc3339Ms);
    });

    test('expires_at wins over expiresAt (client.js:14 order)', () {
      final both = <String, Object?>{
        'expires_at': '2033-01-01T00:00:00Z',
        'expiresAt': '2026-01-02T10:30:00Z',
      };
      expect(HfyGrant.parseExpiryMs(both), DateTime.utc(2033).millisecondsSinceEpoch);
    });

    test('nulls and non-objects → null', () {
      expect(HfyGrant.parseExpiryMs(null), isNull);
      expect(HfyGrant.parseExpiryMs('raw-token'), isNull); // plain string token
      expect(HfyGrant.parseExpiryMs(of(null)), isNull);
      expect(HfyGrant.parseExpiryValue(null), isNull);
      expect(HfyGrant.parseExpiryValue(true), isNull);
      expect(HfyGrant.parseExpiryValue('not a date'), isNull);
    });

    test('hostile numerics → null, never a crash', () {
      expect(HfyGrant.parseExpiryValue(1e300), isNull);
      expect(HfyGrant.parseExpiryValue(double.infinity), isNull);
    });
  });

  group('P-CLIENT-002/P-CLIENT-017 HfyGrant.fromResult (#fetchToken)', () {
    test('a plain string token with no expiry', () {
      final grant = HfyGrant.fromResult('tok123');
      expect(grant.token, 'tok123');
      expect(grant.expiresAt, isNull);
      expect(grant.expiresAtMs, isNull);
    });

    test('{token, expires_at} object', () {
      final grant = HfyGrant.fromResult(<String, Object?>{
        'token': 'tok123',
        'expires_at': '2026-01-02T10:30:00Z',
      });
      expect(grant.token, 'tok123');
      expect(grant.expiresAt, DateTime.utc(2026, 1, 2, 10, 30));
      expect(grant.expiresAtMs, DateTime.utc(2026, 1, 2, 10, 30).millisecondsSinceEpoch);
    });

    test('camelCase {token, expiresAt}', () {
      final grant = HfyGrant.fromResult(<String, Object?>{
        'token': 'tok123',
        'expiresAt': 1727952000.0,
      });
      expect(grant.expiresAtMs, 1727952000000);
    });

    test('missing token / non-map / null → the SAME web error message', () {
      const message = 'onTokenNeeded returned no token';
      expect(() => HfyGrant.fromResult(null), throwsA(isA<HfyProtocolException>().having(
        (e) => e.message, 'message', message,
      )));
      expect(
        () => HfyGrant.fromResult(<String, Object?>{'token': ''}),
        throwsHfyProtocol(),
      );
      expect(() => HfyGrant.fromResult(42), throwsHfyProtocol());
    });

    test('equality and no token leak through toString', () {
      final grant = HfyGrant.fromResult('tok123');
      expect(grant, HfyGrant(token: 'tok123'));
      expect(grant.hashCode, HfyGrant(token: 'tok123').hashCode);
      expect(grant.toString(), isNot(contains('tok123')));
    });
  });
}

Matcher throwsHfyProtocol() => throwsA(isA<HfyProtocolException>());
