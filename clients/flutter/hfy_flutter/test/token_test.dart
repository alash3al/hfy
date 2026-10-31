// Grant expiry tolerance — exact replica of client.js parseExpiryMs
// (client.js:12-25, PARITY.md P-CLIENT-002): expires_at/expiresAt, a Date
// (DateTime in Dart), epoch seconds (< 1e12 → ×1000) or ms, an RFC3339
// string, or a numeric string with the same rule; anything else → null.
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/hfy_flutter.dart';

void main() {
  group('P-CLIENT-002 HfyGrant.parseExpiryMs (parseExpiryMs parity)', () {
    final rfc3339Ms = DateTime.utc(2026, 1, 2, 10, 30).millisecondsSinceEpoch;

    Map<String, Object?> of(Object? expiresAt, {bool camel = false}) =>
        camel ? {'expiresAt': expiresAt} : {'expires_at': expiresAt};

    test('RFC3339 string', () {
      expect(HfyGrant.parseExpiryMs(of('2026-01-02T10:30:00Z')), rfc3339Ms);
      expect(HfyGrant.parseExpiryValue('2026-01-02T10:30:00Z'), rfc3339Ms);
    });

    test('Date-like value (a DateTime instance — the Dart analog of Date)', () {
      expect(
        HfyGrant.parseExpiryValue(DateTime.utc(2026, 1, 2, 10, 30)),
        rfc3339Ms,
      );
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
      expect(
        HfyGrant.parseExpiryMs(of('2026-01-02T10:30:00Z', camel: true)),
        rfc3339Ms,
      );
    });

    test('expires_at wins over expiresAt (client.js:14 order)', () {
      final both = <String, Object?>{
        'expires_at': '2033-01-01T00:00:00Z',
        'expiresAt': '2026-01-02T10:30:00Z',
      };
      expect(
        HfyGrant.parseExpiryMs(both),
        DateTime.utc(2033).millisecondsSinceEpoch,
      );
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

    // The JS-Date representable range is |ms| ≤ 8.64e15 (see the deviation
    // list at the top of lib/src/client/token.dart). PAST it, Dart cannot
    // build a DateTime (RangeError at construction) — an expiry that cannot
    // exist is an expiry that cannot be trusted → null, never an untyped
    // RangeError escaping HfyGrant.fromResult.
    test(
      'far-future numerics are rejected as null, never a RangeError (#1)',
      () {
        expect(HfyGrant.parseExpiryValue(1e16), isNull);
        expect(HfyGrant.parseExpiryValue(9e18), isNull);
        expect(HfyGrant.parseExpiryValue(-1e16), isNull);
        expect(HfyGrant.parseExpiryValue(-9e18), isNull);
        expect(HfyGrant.parseExpiryValue(1e300), isNull);
      },
    );

    test(
      'far-future digit strings flow through the SECONDS/MS heuristic → null',
      () {
        // Note the heuristic first: 99999999999 (11 digits) < 1e12 → ×1000 =
        // 9.99e13 ms (in range, still an instant); only the true far-futures
        // (1e16, 9e18, 17-digit strings) exceed it and yield null.
        expect(
          HfyGrant.parseExpiryValue('99999999999999999'),
          isNull,
          reason:
              'Date.parse would be NaN in JS; the numeric branch gives '
              '9.99e16 ms — past ±8.64e15, unreachable for a DateTime',
        );
        expect(
          HfyGrant.parseExpiryValue('99999999999'),
          99999999999000,
          reason: 'the seconds-vs-ms heuristic before the range check',
        );
      },
    );

    test('the representable bound itself parses (end to end)', () {
      const max = 8640000000000000;
      expect(HfyGrant.parseExpiryValue(max), max);
      expect(
        HfyGrant.parseExpiryValue(-max),
        isNull,
        reason:
            'negatives take the seconds heuristic (< 1e12 → ×1000), '
            'landing past the Date range → null (deviation #2)',
      );
      expect(
        HfyGrant.parseExpiryValue(-1700000000000),
        -1700000000000000,
        reason: 'a plain negative-epoch-seconds expiry stays representable',
      );
      expect(
        HfyGrant.fromResult({'token': 't', 'expires_at': max}).expiresAt,
        DateTime.fromMillisecondsSinceEpoch(max, isUtc: true),
      );
      // One millisecond past it: the token survives, the expiry is dropped —
      // no proactive refresh, no crash (deviation #2 of client.js).
      final past = HfyGrant.fromResult({'token': 't', 'expires_at': max + 1});
      expect(past.token, 't');
      expect(past.expiresAt, isNull);
      expect(past.expiresAtMs, isNull);
    });

    test(
      "''/' whitespace → null, null, null (ACCEPTED deviation #1: JS → 0 = expired)",
      () {
        expect(HfyGrant.parseExpiryValue(''), isNull);
        expect(HfyGrant.parseExpiryValue(' '), isNull);
        expect(
          HfyGrant.fromResult({'token': 't', 'expires_at': ''}).expiresAt,
          isNull,
        );
      },
    );
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
      expect(
        grant.expiresAtMs,
        DateTime.utc(2026, 1, 2, 10, 30).millisecondsSinceEpoch,
      );
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
      expect(
        () => HfyGrant.fromResult(null),
        throwsA(
          isA<HfyProtocolException>().having(
            (e) => e.message,
            'message',
            message,
          ),
        ),
      );
      expect(
        () => HfyGrant.fromResult(<String, Object?>{'token': ''}),
        throwsA(
          isA<HfyProtocolException>().having(
            (e) => e.message,
            'message',
            message,
          ),
        ),
      );
      expect(() => HfyGrant.fromResult(42), throwsHfyProtocol());
    });

    // Stricter Dart typing KEPT (the token must be a String, like web's
    // `typeof result.token === 'string'` guard) — but the hard error is now
    // always the JS message, not a Dart-flavored side-channel.
    test(
      'a NON-STRING token value → the SAME JS message (no side-channel)',
      () {
        const message = 'onTokenNeeded returned no token';
        for (final badToken in const [
          42,
          true,
          ['t'],
          <String, Object?>{},
        ]) {
          expect(
            () => HfyGrant.fromResult({'token': badToken}),
            throwsA(
              isA<HfyProtocolException>().having(
                (e) => e.message,
                'message',
                message,
              ),
            ),
            reason: 'token ${badToken.runtimeType}',
          );
        }
      },
    );

    test('equality and no token leak through toString', () {
      final grant = HfyGrant.fromResult('tok123');
      expect(grant, HfyGrant(token: 'tok123'));
      expect(grant.hashCode, HfyGrant(token: 'tok123').hashCode);
      expect(grant.toString(), isNot(contains('tok123')));
    });
  });
}

Matcher throwsHfyProtocol() => throwsA(isA<HfyProtocolException>());
