// Colour math edge cases (P-VIS-005/006 colour-mix rules incl. the
// premultiplied-alpha rule for transparent bases, P-VIS-007 readability,
// P-VIS-022 hue conversion). Pure: no widgets.

import 'package:flutter/material.dart' show Brightness, Color;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/theme/accent.dart';
import 'package:hfy_flutter/src/theme/theme.dart';

void main() {
  const white = HfyColor.hex(0xffffff);
  const ink = HfyColor.hex(0x111827);
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

  // ui.js:155-156 decides with L = 0.2126 R + 0.7152 G + 0.0722 B over linear
  // channels, lin(c) = c / 12.92 up to 0.03928 else ((c + 0.055) / 1.055)^2.4,
  // and white iff 1.05 / (L + 0.05) >= 4, i.e. L <= 0.2125. Channels are v/255.
  group('P-VIS-007 the 4:1 cut-off, decided exactly as ui.js does', () {
    test('#7f7f7f clears 4:1 and keeps white', () {
      // v=127: c=0.498039, a grey so L = lin = 0.2122308
      //   1.05 / 0.2622308 = 4.004106 >= 4 -> white
      expect(hfyOnColor(const HfyColor.hex(0x7f7f7f)), white);
    });

    test('#808080 misses 4:1 and takes ink', () {
      // v=128: c=0.501961, a grey so L = lin = 0.2158605
      //   1.05 / 0.2658605 = 3.949440 < 4 -> ink
      expect(hfyOnColor(const HfyColor.hex(0x808080)), ink);
    });

    test(
      '#888214 (L 0.2125001) takes ink, #6e8912 (L 0.2124999) keeps white',
      () {
        // #888214: lin(136)=0.2462013, lin(130)=0.2232280, lin(20)=0.0069954
        //   L = 0.2126*0.2462013 + 0.7152*0.2232280 + 0.0722*0.0069954
        //     = 0.2125001
        //   1.05 / 0.2625001 = 3.9999985 < 4 -> ink
        expect(hfyOnColor(const HfyColor.hex(0x888214)), ink);
        // #6e8912: lin(110)=0.1559265, lin(137)=0.2501583, lin(18)=0.0060488
        //   L = 0.2124999, 1.05 / 0.2624999 = 4.0000015 >= 4 -> white
        expect(hfyOnColor(const HfyColor.hex(0x6e8912)), white);
      },
    );

    test(
      'a blue channel of 4 takes the linear branch: #6a8a04 keeps white',
      () {
        // b=4: c=0.015686 <= 0.03928 -> lin = 0.015686 / 12.92 = 0.0012141
        //   r=106: lin 0.1441285, g=138: lin 0.2541521
        //   L = 0.2126*0.1441285 + 0.7152*0.2541521 + 0.0722*0.0012141
        //     = 0.21249895
        //   1.05 / 0.26249895 = 4.000016 >= 4 -> white
        expect(hfyOnColor(const HfyColor.hex(0x6a8a04)), white);
      },
    );

    test('a green channel of 4 takes the linear branch: #fc0448 takes ink', () {
      // g=4: lin = 0.0012141 (linear); r=252: lin 0.9734453; b=72: lin 0.0648033
      //   L = 0.2125016, 1.05 / 0.2625016 = 3.999976 < 4 -> ink
      expect(hfyOnColor(const HfyColor.hex(0xfc0448)), ink);
    });

    test('a green channel of 11 takes the power branch: #f40b88 takes ink', () {
      // g=11: c=0.043137 > 0.03928 -> lin = ((0.043137+0.055)/1.055)^2.4
      //   = 0.0033465; r=244: lin 0.9046612; b=136: lin 0.2462013
      //   L = 0.2125001, 1.05 / 0.2625001 = 3.9999985 < 4 -> ink
      expect(hfyOnColor(const HfyColor.hex(0xf40b88)), ink);
    });

    test('a channel inside the knee band (0.04) takes the power curve', () {
      // Not an 8-bit level, so no hex accent reaches it. It pins the curve
      // between 0.03928 and 0.04045, where no v/255 falls.
      // lin(0.04) = ((0.04+0.055)/1.055)^2.4 = 0.0030955, so 0.2126*lin = 0.0006581
      // g=0.5804468: lin = 0.2961994, so 0.7152*lin = 0.2118418
      //   L = 0.21249993, 1.05 / 0.26249993 = 4.000001 >= 4 -> white
      // A 0.04045 knee gives lin(0.04) = 0.0030960 and contrast 3.9999995 -> ink.
      expect(hfyOnColor(const HfyColor(0.04, 0.5804468282, 0)), white);
    });

    test('pure black keeps white at 21:1, pure white takes ink at 1:1', () {
      // L = 0 -> 1.05 / 0.05 = 21; L = 1 -> 1.05 / 1.05 = 1
      expect(hfyOnColor(const HfyColor.hex(0x000000)), white);
      expect(hfyOnColor(const HfyColor.hex(0xffffff)), ink);
    });
  });

  group('hex input and the default accent feed the cut-off', () {
    test('#F00 expands to #ff0000, whose L 0.2126 gives 3.998 and takes ink', () {
      // 3-digit hex doubles each digit (ui.js:150); L = 0.2126 -> 1.05 / 0.2626 = 3.998477
      expect(HfyColor.parseHex('#F00'), const HfyColor.hex(0xff0000));
      expect(hfyOnColor(HfyColor.parseHex('#F00')!), ink);
    });

    test('#FFF and #000 expand to pure white and black', () {
      expect(HfyColor.parseHex('#FFF'), white);
      expect(hfyOnColor(HfyColor.parseHex('#fff')!), ink);
      expect(hfyOnColor(HfyColor.parseHex('#000')!), white);
    });

    test(
      'an accent that does not parse falls back to #6366f1 with white text',
      () {
        // ui.js would hand the bad value to --hfy-accent; Dart takes the default.
        // #6366f1: L = 0.1850627, 1.05 / 0.2350627 = 4.466895 -> white
        for (final bad in ['red', '#ffff', 'not-a-colour', '']) {
          final t = HfyTheme.resolve(
            platformBrightness: Brightness.light,
            reduceMotion: false,
            accent: bad,
          );
          expect(t.tokens.accent, const Color(0xff6366f1), reason: bad);
          expect(t.tokens.onAccent, const Color(0xffffffff), reason: bad);
        }
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
