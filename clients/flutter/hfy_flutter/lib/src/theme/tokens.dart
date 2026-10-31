// The embed's design tokens (ui.css:1-67, measured in a real browser into
// clients/fixtures/tokens.json): 23 colours, 2 shadows and the sizing scale,
// derived from (accent, brightness, density) with ui.css's color-mix rules.
// Widgets read them through HfyTheme (theme.dart).

import 'dart:math' as math;
import 'dart:ui' show Brightness;

import 'package:flutter/foundation.dart' show listEquals;
import 'package:flutter/painting.dart' show BoxShadow, Color, Offset;

import 'accent.dart';

enum HfyDensity { comfortable, compact }

/// The token set. It imports dart:ui and package:flutter (Color, BoxShadow), so
/// it cannot compile to JS, and test/purity_test.dart does not guard it. If the
/// tokens must be shared with the JS core, the route is a Flutter-free token
/// map (names to HfyColor and sizes) that this file turns into Color and
/// BoxShadow.
class HfyTokens {
  HfyTokens._({
    required HfyColor accent,
    required HfyColor onAccent,
    required HfyColor bg,
    required HfyColor surface,
    required HfyColor surface2,
    required HfyColor surface3,
    required HfyColor border,
    required HfyColor text,
    required HfyColor muted,
    required HfyColor accentSoft,
    required HfyColor accentText,
    required HfyColor ring,
    required HfyColor own,
    required HfyColor wall,
    required HfyColor wallDot,
    required HfyColor ok,
    required HfyColor okSoft,
    required HfyColor err,
    required HfyColor errSoft,
    required HfyColor warn,
    required HfyColor warnSoft,
    required HfyColor tickRead,
    required HfyColor scrim,
    required this.shadow1,
    required this.shadow2,
    required this.senderHues,
    required this.fontSize,
    required this.lineHeight,
    required this.gap,
    required this.gapTight,
    required this.padY,
    required this.padX,
    required this.avatar,
    required this.maxWidth,
    required this.footWidth,
    required this.footWidthOwn,
    required this.gutterMin,
    required this.sheetGutterMin,
    required this.sheetMaxWidth,
  }) : accent = _c(accent),
       onAccent = _c(onAccent),
       bg = _c(bg),
       surface = _c(surface),
       surface2 = _c(surface2),
       surface3 = _c(surface3),
       border = _c(border),
       text = _c(text),
       muted = _c(muted),
       accentSoft = _c(accentSoft),
       accentText = _c(accentText),
       ring = _c(ring),
       own = _c(own),
       wall = _c(wall),
       wallDot = _c(wallDot),
       ok = _c(ok),
       okSoft = _c(okSoft),
       err = _c(err),
       errSoft = _c(errSoft),
       warn = _c(warn),
       warnSoft = _c(warnSoft),
       tickRead = _c(tickRead),
       scrim = _c(scrim);

  static const double s1 = 4;
  static const double s2 = 8;
  static const double s3 = 12;
  static const double s4 = 16;
  static const double s5 = 20;
  static const double s6 = 24;
  static const double r1 = 6;
  static const double r2 = 10;
  static const double r3 = 14;
  static const double r4 = 18;

  final Color accent;
  final Color onAccent;
  final Color bg;
  final Color surface;
  final Color surface2;
  final Color surface3;
  final Color border;
  final Color text;
  final Color muted;
  final Color accentSoft;
  final Color accentText;
  final Color ring;
  final Color own;
  final Color wall;
  final Color wallDot;
  final Color ok;
  final Color okSoft;
  final Color err;
  final Color errSoft;
  final Color warn;
  final Color warnSoft;
  final Color tickRead;
  final Color scrim;
  final List<BoxShadow> shadow1;
  final List<BoxShadow> shadow2;

  /// The twelve sender-name hues (ui.css:161-167), bucketed by the caller.
  final List<Color> senderHues;

  final double fontSize;
  final double lineHeight;
  final double gap;
  final double gapTight;
  final double padY;
  final double padX;
  final double avatar;
  final double maxWidth;
  final double footWidth;
  final double footWidthOwn;
  final double gutterMin;
  final double sheetGutterMin;
  final double sheetMaxWidth;

  /// `--hfy-gutter` at a widget width: the column stays centred at maxWidth.
  double gutterFor(double width) => math.max(gutterMin, (width - maxWidth) / 2);

  double sheetGutterFor(double width) =>
      math.max(sheetGutterMin, (width - sheetMaxWidth) / 2);

  List<Object> get _values => [
    accent,
    onAccent,
    bg,
    surface,
    surface2,
    surface3,
    border,
    text,
    muted,
    accentSoft,
    accentText,
    ring,
    own,
    wall,
    wallDot,
    ok,
    okSoft,
    err,
    errSoft,
    warn,
    warnSoft,
    tickRead,
    scrim,
    fontSize,
    lineHeight,
    gap,
    gapTight,
    padY,
    padX,
    avatar,
    maxWidth,
    footWidth,
    footWidthOwn,
    gutterMin,
    sheetGutterMin,
    sheetMaxWidth,
  ];

  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is HfyTokens &&
          listEquals(_values, other._values) &&
          listEquals(shadow1, other.shadow1) &&
          listEquals(shadow2, other.shadow2) &&
          listEquals(senderHues, other.senderHues);

  @override
  int get hashCode => Object.hash(
    Object.hashAll(_values),
    Object.hashAll(shadow1),
    Object.hashAll(shadow2),
    Object.hashAll(senderHues),
  );

  factory HfyTokens.build({
    required HfyColor accent,
    required Brightness brightness,
    required HfyDensity density,
  }) {
    final dark = brightness == Brightness.dark;
    final compact = density == HfyDensity.compact;
    T pick<T>(T light, T night) => dark ? night : light;
    HfyColor mix(double pct, HfyColor base) => mixSrgb(accent, pct, base);
    HfyColor hex(int rgb, [double alpha = 1]) => HfyColor.hex(rgb, alpha);
    const none = HfyColor.transparent;
    BoxShadow shadow(HfyColor color, double dy, double blur) =>
        BoxShadow(color: _c(color), offset: Offset(0, dy), blurRadius: blur);
    final hues = pick(_hueLight, _hueDark);

    return HfyTokens._(
      accent: accent,
      onAccent: hfyOnColor(accent),
      bg: pick(mix(3, hex(0xf6f7f9)), mix(3, hex(0x0e1015))),
      surface: pick(hex(0xffffff), mix(3, hex(0x1b1e26))),
      surface2: pick(mix(4, hex(0xf1f3f6)), mix(5, hex(0x22262f))),
      surface3: pick(mix(7, hex(0xe7eaf0)), mix(8, hex(0x2c313c))),
      border: pick(mix(7, hex(0xe1e4ea)), mix(9, hex(0x2f3440))),
      text: pick(hex(0x151821), hex(0xe9ebf1)),
      muted: pick(hex(0x596070), hex(0xa3aab9)),
      accentSoft: pick(mix(11, hex(0xffffff)), mix(20, hex(0x1b1e26))),
      accentText: pick(mix(58, hex(0x000000)), mix(45, hex(0xffffff))),
      ring: pick(mix(40, none), mix(55, none)),
      own: pick(mix(15, hex(0xffffff)), mix(32, hex(0x1b1e26))),
      wall: pick(mix(5, hex(0xeceef1)), mix(3, hex(0x0c0e13))),
      wallDot: pick(mix(13, none), mix(12, none)),
      ok: pick(hex(0x15803d), hex(0x4ade80)),
      okSoft: pick(hex(0xe7f6ec), hex(0x14261b)),
      err: pick(hex(0xb91c1c), hex(0xf87171)),
      errSoft: pick(hex(0xfdecec), hex(0x2c1717)),
      warn: pick(hex(0xa16207), hex(0xfbbf24)),
      warnSoft: pick(hex(0xfdf6e3), hex(0x2a2210)),
      tickRead: pick(hex(0x0b89d1), hex(0x53bdeb)),
      scrim: pick(hex(0x0b141a, 0.45), hex(0x000000, 0.62)),
      shadow1: pick(
        [shadow(hex(0x0b141a, 0.13), 1, 0.5)],
        [shadow(hex(0x000000, 0.4), 1, 0.5)],
      ),
      shadow2: pick(
        [shadow(hex(0x101828, 0.08), 6, 16), shadow(hex(0x101828, 0.05), 2, 4)],
        [shadow(hex(0x000000, 0.35), 8, 24), shadow(hex(0x000000, 0.25), 2, 4)],
      ),
      senderHues: [for (final h in hues) _c(hfyHsl(h.$1, h.$2, h.$3))],
      fontSize: compact ? 14 : 15,
      lineHeight: compact ? 1.42 : 1.5,
      gap: compact ? 8 : 12,
      gapTight: 2,
      padY: compact ? 5 : 7,
      padX: compact ? 9 : 10,
      avatar: compact ? 26 : 30,
      maxWidth: 820,
      footWidth: 58,
      footWidthOwn: 76,
      gutterMin: 16,
      sheetGutterMin: 20,
      sheetMaxWidth: 860,
    );
  }
}

Color _c(HfyColor c) => Color.from(alpha: c.a, red: c.r, green: c.g, blue: c.b);

const _hueLight = <(double, double, double)>[
  (0, 62, 42),
  (28, 80, 34),
  (45, 90, 28),
  (95, 55, 30),
  (150, 60, 29),
  (178, 70, 28),
  (200, 75, 35),
  (220, 65, 46),
  (255, 50, 52),
  (285, 50, 45),
  (320, 58, 42),
  (345, 65, 44),
];

const _hueDark = <(double, double, double)>[
  (0, 80, 72),
  (28, 85, 66),
  (45, 85, 62),
  (95, 55, 62),
  (150, 55, 60),
  (178, 60, 58),
  (200, 80, 68),
  (220, 85, 74),
  (255, 80, 78),
  (285, 65, 74),
  (320, 70, 74),
  (345, 80, 74),
];
