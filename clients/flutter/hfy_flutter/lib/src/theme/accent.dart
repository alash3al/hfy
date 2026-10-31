// Pure sRGB colour math behind the embed's CSS tokens (ui.css:39-64), the
// accent readability rule (ui.js:147-157) and the sender hue table
// (ui.css:161-167). No Flutter imports: this file stays shareable with the
// JS-compiled core (test/purity_test.dart enforces it).

import 'dart:math' as math;

/// An sRGB colour with gamma-encoded channels and alpha, all in 0..1: the same
/// numbers CSS serialises as `color(srgb r g b / a)`.
class HfyColor {
  const HfyColor(this.r, this.g, this.b, [this.a = 1]);

  /// `0xRRGGBB` with an optional alpha in 0..1.
  const HfyColor.hex(int rgb, [double alpha = 1])
    : this(
        ((rgb >> 16) & 0xff) / 255,
        ((rgb >> 8) & 0xff) / 255,
        (rgb & 0xff) / 255,
        alpha,
      );

  static const transparent = HfyColor(0, 0, 0, 0);

  final double r;
  final double g;
  final double b;
  final double a;

  @override
  bool operator ==(Object other) =>
      other is HfyColor &&
      r == other.r &&
      g == other.g &&
      b == other.b &&
      a == other.a;

  @override
  int get hashCode => Object.hash(r, g, b, a);

  @override
  String toString() => 'HfyColor($r, $g, $b, $a)';

  /// `#rgb` or `#rrggbb` (ui.js:147-148 takes the `#` optionally); null otherwise.
  static HfyColor? parseHex(String raw) {
    final m = _hex.firstMatch(raw.trim());
    if (m == null) return null;
    var digits = m.group(1)!;
    if (digits.length == 3) digits = digits.split('').map((c) => '$c$c').join();
    return HfyColor.hex(int.parse(digits, radix: 16));
  }

  static final _hex = RegExp(
    r'^#?([0-9a-f]{3}|[0-9a-f]{6})$',
    caseSensitive: false,
  );
}

/// CSS `color-mix(in srgb, a pct%, b)`: premultiplied sRGB interpolation, so an
/// opaque pair blends channel by channel while a transparent `b` keeps a's RGB
/// and only scales its alpha to pct% (no channel darkens toward black).
HfyColor mixSrgb(HfyColor a, double pct, HfyColor b) {
  final wa = pct / 100;
  final wb = 1 - wa;
  final alpha = wa * a.a + wb * b.a;
  if (alpha == 0) return HfyColor.transparent;
  double channel(double ca, double cb) =>
      (wa * ca * a.a + wb * cb * b.a) / alpha;
  return HfyColor(
    channel(a.r, b.r),
    channel(a.g, b.g),
    channel(a.b, b.b),
    alpha,
  );
}

/// ui.js:147-157: white on the accent unless its WCAG contrast against white
/// would fall under 4:1, then the dark ink #111827.
HfyColor hfyOnColor(HfyColor c) {
  double linear(double v) =>
      v <= 0.03928 ? v / 12.92 : math.pow((v + 0.055) / 1.055, 2.4).toDouble();
  final lum =
      0.2126 * linear(c.r) + 0.7152 * linear(c.g) + 0.0722 * linear(c.b);
  return 1.05 / (lum + 0.05) >= 4
      ? const HfyColor.hex(0xffffff)
      : const HfyColor.hex(0x111827);
}

/// CSS `hsl(h s% l%)` converted to sRGB, for the sender hue palette.
HfyColor hfyHsl(double h, double s, double l) {
  final sat = s / 100;
  final lit = l / 100;
  final c = (1 - (2 * lit - 1).abs()) * sat;
  final hp = (h % 360) / 60;
  final x = c * (1 - ((hp % 2) - 1).abs());
  final m = lit - c / 2;
  final (r, g, b) = switch (hp.floor()) {
    0 => (c, x, 0.0),
    1 => (x, c, 0.0),
    2 => (0.0, c, x),
    3 => (0.0, x, c),
    4 => (x, 0.0, c),
    _ => (c, 0.0, x),
  };
  return HfyColor(r + m, g + m, b + m);
}
