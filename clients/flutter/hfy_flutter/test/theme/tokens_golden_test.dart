// Browser-measured golden parity (P-VIS-001..009): every colour, shadow and
// sizing value that clients/fixtures/tokens.json records (read from the live
// /embed page with getComputedStyle) must equal HfyTokens within 1/255 per
// colour channel and 0.005 alpha.

import 'dart:convert' show jsonDecode;
import 'dart:io' show File;
import 'dart:math' as math;
import 'dart:ui' show Brightness;

import 'package:flutter/foundation.dart' show debugPrint;
import 'package:flutter/painting.dart' show Color, Offset;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/theme/accent.dart';
import 'package:hfy_flutter/src/theme/tokens.dart';

import '../helpers/fixture_loader.dart';

final Map<String, dynamic> _golden =
    jsonDecode(File('${fixturesDir().path}/tokens.json').readAsStringSync())
        as Map<String, dynamic>;

final Map<String, Color Function(HfyTokens)> _colorVars = {
  '--hfy-accent': (t) => t.accent,
  '--hfy-on-accent': (t) => t.onAccent,
  '--hfy-bg': (t) => t.bg,
  '--hfy-surface': (t) => t.surface,
  '--hfy-surface-2': (t) => t.surface2,
  '--hfy-surface-3': (t) => t.surface3,
  '--hfy-border': (t) => t.border,
  '--hfy-text': (t) => t.text,
  '--hfy-muted': (t) => t.muted,
  '--hfy-accent-soft': (t) => t.accentSoft,
  '--hfy-accent-text': (t) => t.accentText,
  '--hfy-ring': (t) => t.ring,
  '--hfy-own': (t) => t.own,
  '--hfy-wall': (t) => t.wall,
  '--hfy-wall-dot': (t) => t.wallDot,
  '--hfy-ok': (t) => t.ok,
  '--hfy-ok-soft': (t) => t.okSoft,
  '--hfy-err': (t) => t.err,
  '--hfy-err-soft': (t) => t.errSoft,
  '--hfy-warn': (t) => t.warn,
  '--hfy-warn-soft': (t) => t.warnSoft,
  '--hfy-tick-read': (t) => t.tickRead,
  '--hfy-scrim': (t) => t.scrim,
};

/// A CSS colour as the browser serialised it (channels and alpha in 0..1).
class _Css {
  const _Css(this.r, this.g, this.b, this.a);
  final double r;
  final double g;
  final double b;
  final double a;
}

/// `color(srgb r g b[ / a])` holds 0..1 floats, `rgb()`/`rgba()` holds 0..255.
_Css _parseCss(String css) {
  final srgb = RegExp(r'^color\(srgb ([^)]*)\)$').firstMatch(css);
  if (srgb != null) {
    final parts = srgb.group(1)!.split(' / ');
    final c = parts[0].split(' ').map(double.parse).toList();
    return _Css(
      c[0],
      c[1],
      c[2],
      parts.length > 1 ? double.parse(parts[1]) : 1,
    );
  }
  final rgb = RegExp(r'^rgba?\(([^)]*)\)$').firstMatch(css)!;
  final v = rgb
      .group(1)!
      .split(',')
      .map((s) => double.parse(s.trim()))
      .toList();
  return _Css(v[0] / 255, v[1] / 255, v[2] / 255, v.length > 3 ? v[3] : 1);
}

double _channelDelta(Color mine, _Css want) =>
    [
      (mine.r - want.r).abs(),
      (mine.g - want.g).abs(),
      (mine.b - want.b).abs(),
    ].reduce(math.max) *
    255;

final _shadowPattern = RegExp(
  r'(rgba?\([^)]*\)) (-?[\d.]+)px (-?[\d.]+)px (-?[\d.]+)px (-?[\d.]+)px',
);

double _px(String v) => double.parse(v.replaceAll('px', ''));

void main() {
  final accents = (_golden['accents'] as List<dynamic>).cast<String>();
  final themes = _golden['themes'] as Map<String, dynamic>;

  test(
    'P-VIS-003..006 the fixture lists the same colour and shadow variables',
    () {
      final colors =
          (themes['light'] as Map<String, dynamic>)[accents.first]
              as Map<String, dynamic>;
      expect(
        (colors['colors'] as Map<String, dynamic>).keys.toSet(),
        _colorVars.keys.toSet(),
      );
      expect(_colorVars, hasLength(23));
    },
  );

  for (final theme in ['light', 'dark']) {
    for (final accent in accents) {
      test(
        'P-VIS-003..006 $theme $accent: 23 colours and 2 shadows match tokens.json',
        () {
          final brightness = theme == 'dark'
              ? Brightness.dark
              : Brightness.light;
          final tokens = HfyTokens.build(
            accent: HfyColor.parseHex(accent)!,
            brightness: brightness,
            density: HfyDensity.comfortable,
          );
          final golden =
              (themes[theme] as Map<String, dynamic>)[accent]
                  as Map<String, dynamic>;
          final colors = golden['colors'] as Map<String, dynamic>;
          var maxChannel = 0.0;
          var maxAlpha = 0.0;
          for (final entry in _colorVars.entries) {
            final want = _parseCss(colors[entry.key] as String);
            final mine = entry.value(tokens);
            maxChannel = math.max(maxChannel, _channelDelta(mine, want));
            maxAlpha = math.max(maxAlpha, (mine.a - want.a).abs());
          }
          expect(
            maxChannel,
            lessThanOrEqualTo(1),
            reason: '$theme $accent colour channel delta',
          );
          expect(
            maxAlpha,
            lessThanOrEqualTo(0.005),
            reason: '$theme $accent alpha delta',
          );

          final shadows = golden['shadows'] as Map<String, dynamic>;
          var maxShadow = 0.0;
          for (final (key, list) in [
            ('--hfy-shadow-1', tokens.shadow1),
            ('--hfy-shadow-2', tokens.shadow2),
          ]) {
            final matches = _shadowPattern
                .allMatches(shadows[key] as String)
                .toList();
            expect(matches, hasLength(list.length), reason: key);
            for (var i = 0; i < matches.length; i++) {
              final m = matches[i];
              final want = _parseCss(m.group(1)!);
              final mine = list[i];
              expect(
                mine.offset,
                Offset(_px(m.group(2)!), _px(m.group(3)!)),
                reason: '$key offset',
              );
              expect(mine.blurRadius, _px(m.group(4)!), reason: '$key blur');
              expect(
                mine.spreadRadius,
                _px(m.group(5)!),
                reason: '$key spread',
              );
              maxShadow = math.max(maxShadow, _channelDelta(mine.color, want));
              expect(
                (mine.color.a - want.a).abs(),
                lessThanOrEqualTo(0.005),
                reason: '$key alpha',
              );
            }
          }
          expect(
            maxShadow,
            lessThanOrEqualTo(1),
            reason: '$theme $accent shadow channel delta',
          );
          debugPrint(
            'tokens.json $theme $accent: max channel ${maxChannel.toStringAsFixed(4)}/255, max alpha ${maxAlpha.toStringAsFixed(5)}',
          );
        },
      );
    }
  }

  test('P-VIS-001 comfortable and compact sizing tokens match tokens.json', () {
    final density = _golden['density'] as Map<String, dynamic>;
    for (final entry in {
      'comfortable': HfyDensity.comfortable,
      'compact': HfyDensity.compact,
    }.entries) {
      final want = density[entry.key] as Map<String, dynamic>;
      final t = HfyTokens.build(
        accent: const HfyColor.hex(0x6366f1),
        brightness: Brightness.light,
        density: entry.value,
      );
      expect(
        t.fontSize,
        _px(want['--hfy-fs'] as String),
        reason: '${entry.key} fs',
      );
      expect(
        t.lineHeight,
        double.parse(want['--hfy-lh'] as String),
        reason: '${entry.key} lh',
      );
      expect(
        t.gap,
        _px(want['--hfy-gap'] as String),
        reason: '${entry.key} gap',
      );
      expect(
        t.gapTight,
        _px(want['--hfy-gap-tight'] as String),
        reason: '${entry.key} gap-tight',
      );
      expect(
        t.padY,
        _px(want['--hfy-pad-y'] as String),
        reason: '${entry.key} pad-y',
      );
      expect(
        t.padX,
        _px(want['--hfy-pad-x'] as String),
        reason: '${entry.key} pad-x',
      );
      expect(
        t.avatar,
        _px(want['--hfy-avatar'] as String),
        reason: '${entry.key} avatar',
      );
      expect(
        t.maxWidth,
        _px(want['--hfy-max'] as String),
        reason: '${entry.key} max',
      );
      expect(
        t.footWidth,
        _px(want['--hfy-foot-w'] as String),
        reason: '${entry.key} foot',
      );

      final gutter = RegExp(
        r'^max\((\d+)px,0px,0px,calc\(\(100% - (\d+)px\) / 2\)\)$',
      ).firstMatch(want['--hfy-gutter'] as String)!;
      expect(t.gutterMin, double.parse(gutter.group(1)!));
      expect(t.maxWidth, double.parse(gutter.group(2)!));
      final sheet = RegExp(
        r'^max\((\d+)px,0px,0px,calc\(\(100% - (\d+)px\) / 2\)\)$',
      ).firstMatch(want['--hfy-sheet-gutter'] as String)!;
      expect(t.sheetGutterMin, double.parse(sheet.group(1)!));
      expect(t.sheetMaxWidth, double.parse(sheet.group(2)!));
    }
  });

  test('P-VIS-001 the gutter formula keeps the column centred at 820px', () {
    final t = HfyTokens.build(
      accent: const HfyColor.hex(0x6366f1),
      brightness: Brightness.light,
      density: HfyDensity.comfortable,
    );
    expect(t.gutterFor(360), 16, reason: 'narrow widths keep the 16px minimum');
    expect(t.gutterFor(1020), 100, reason: '(1020 - 820) / 2');
    expect(t.sheetGutterFor(1000), 70, reason: '(1000 - 860) / 2');
  });

  test('P-VIS-001 spacing scale and radii equal ui.css:6-7', () {
    expect(
      [
        HfyTokens.s1,
        HfyTokens.s2,
        HfyTokens.s3,
        HfyTokens.s4,
        HfyTokens.s5,
        HfyTokens.s6,
      ],
      [4, 8, 12, 16, 20, 24],
    );
    expect(
      [HfyTokens.r1, HfyTokens.r2, HfyTokens.r3, HfyTokens.r4],
      [6, 10, 14, 18],
    );
  });
}
