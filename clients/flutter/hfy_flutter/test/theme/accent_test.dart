// Colour math edge cases (P-VIS-005/006 colour-mix rules incl. the
// premultiplied-alpha rule for transparent bases, P-VIS-007 readability,
// P-VIS-022 hue conversion). Pure: no widgets.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/theme/accent.dart';

void main() {
  const white = HfyColor.hex(0xffffff);
  const accent = HfyColor.hex(0x0ea5e9);

  group('P-VIS-005 mixSrgb matches CSS color-mix(in srgb, a p%, b)', () {
    test('0% gives b and 100% gives a, exactly', () {
      final zero = mixSrgb(accent, 0, white);
      expect(
        [zero.r, zero.g, zero.b, zero.a],
        [white.r, white.g, white.b, white.a],
      );
      final full = mixSrgb(accent, 100, white);
      expect(
        [full.r, full.g, full.b, full.a],
        [accent.r, accent.g, accent.b, accent.a],
      );
    });

    test('opaque base blends channel by channel in sRGB', () {
      final m = mixSrgb(accent, 11, white);
      expect(m.r, closeTo(0.11 * accent.r + 0.89, 1e-12));
      expect(m.g, closeTo(0.11 * accent.g + 0.89, 1e-12));
      expect(m.b, closeTo(0.11 * accent.b + 0.89, 1e-12));
      expect(m.a, 1);
    });

    test(
      'transparent base keeps the accent RGB and scales alpha (ring 40%)',
      () {
        final ring = mixSrgb(accent, 40, HfyColor.transparent);
        expect([ring.r, ring.g, ring.b], [accent.r, accent.g, accent.b]);
        expect(ring.a, closeTo(0.4, 1e-12));
      },
    );

    test(
      'transparent accent keeps the base RGB and scales alpha of the base',
      () {
        final m = mixSrgb(HfyColor.transparent, 40, white);
        expect([m.r, m.g, m.b], [1, 1, 1]);
        expect(m.a, closeTo(0.6, 1e-12));
      },
    );

    test('two transparent colours give transparent', () {
      expect(mixSrgb(HfyColor.transparent, 50, HfyColor.transparent).a, 0);
    });
  });

  group('P-VIS-007 hfyOnColor (ui.js:147-157)', () {
    test(
      'accents with 4:1 or better against white keep white; the rest take #111827',
      () {
        expect(hfyOnColor(const HfyColor.hex(0x6366f1)), white);
        expect(hfyOnColor(const HfyColor.hex(0xe11d48)), white);
        expect(hfyOnColor(accent), const HfyColor.hex(0x111827));
        expect(
          hfyOnColor(const HfyColor.hex(0x25d366)),
          const HfyColor.hex(0x111827),
        );
        expect(
          hfyOnColor(const HfyColor.hex(0xf59e0b)),
          const HfyColor.hex(0x111827),
        );
      },
    );
  });

  group('hex parsing (ui.js:147-148)', () {
    test('accepts #rgb and #rrggbb with or without the hash', () {
      expect(HfyColor.parseHex('#abc'), const HfyColor.hex(0xaabbcc));
      expect(HfyColor.parseHex(' 6366F1 '), const HfyColor.hex(0x6366f1));
    });

    test('rejects everything else', () {
      expect(HfyColor.parseHex('red'), isNull);
      expect(HfyColor.parseHex('#12345'), isNull);
      expect(HfyColor.parseHex(''), isNull);
    });
  });

  group('P-VIS-022 hfyHsl (CSS hsl() to sRGB)', () {
    test('primaries and grey', () {
      expect(hfyHsl(0, 100, 50), const HfyColor.hex(0xff0000));
      expect(hfyHsl(120, 100, 50), const HfyColor.hex(0x00ff00));
      expect(hfyHsl(240, 100, 50), const HfyColor.hex(0x0000ff));
      expect(hfyHsl(300, 100, 50), const HfyColor.hex(0xff00ff));
      expect(hfyHsl(0, 0, 50).r, closeTo(0.5, 1e-12));
    });
  });
}
