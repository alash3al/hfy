// HfyTheme as a ThemeExtension: value equality (ThemeData compares its
// extensions with ==), copyWith and lerp, plus HfyTokens equality.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/theme/accent.dart';
import 'package:hfy_flutter/src/theme/theme.dart';
import 'package:hfy_flutter/src/theme/tokens.dart';

Widget _scope(void Function(BuildContext context) seen) => MediaQuery(
  data: const MediaQueryData(),
  child: HfyThemeScope(
    lang: 'en',
    child: Builder(
      builder: (context) {
        seen(context);
        return const SizedBox();
      },
    ),
  ),
);

void main() {
  final base = HfyTheme.resolve(
    platformBrightness: Brightness.light,
    reduceMotion: false,
    lang: 'en',
  );

  group('value equality', () {
    test('the same config gives equal themes with equal hash codes', () {
      final again = HfyTheme.resolve(
        platformBrightness: Brightness.light,
        reduceMotion: false,
        lang: 'en',
      );
      expect(identical(again, base), isFalse);
      expect(again, base);
      expect(again.hashCode, base.hashCode);
    });

    test('every field takes part in equality', () {
      final changed = [
        base.copyWith(
          tokens: HfyTokens.build(
            accent: const HfyColor.hex(0x0ea5e9),
            brightness: Brightness.light,
            density: HfyDensity.comfortable,
          ),
        ),
        base.copyWith(brightness: Brightness.dark),
        base.copyWith(density: HfyDensity.compact),
        base.copyWith(layout: HfyLayout.transcript),
        base.copyWith(lang: 'ar'),
        base.copyWith(textDirection: TextDirection.rtl),
        base.copyWith(reduceMotion: true),
      ];
      for (final t in changed) {
        expect(t, isNot(base));
      }
    });

    test('tokens compare by value and differ by accent, theme or density', () {
      HfyTokens build({
        String accent = '#6366f1',
        Brightness brightness = Brightness.light,
        HfyDensity density = HfyDensity.comfortable,
      }) => HfyTokens.build(
        accent: HfyColor.parseHex(accent)!,
        brightness: brightness,
        density: density,
      );
      final first = build();
      expect(build(), first);
      expect(build().hashCode, first.hashCode);
      expect(build(accent: '#0ea5e9'), isNot(first));
      expect(build(brightness: Brightness.dark), isNot(first));
      expect(build(density: HfyDensity.compact), isNot(first));
    });
  });

  group('copyWith', () {
    test('with no arguments gives an equal copy', () {
      expect(base.copyWith(), base);
    });

    test('replaces only the named fields', () {
      final ar = base.copyWith(lang: 'ar', textDirection: TextDirection.rtl);
      expect(
        [ar.lang, ar.textDirection, ar.brightness, ar.density, ar.tokens],
        ['ar', TextDirection.rtl, base.brightness, base.density, base.tokens],
      );
    });
  });

  group('lerp', () {
    test('keeps this theme below t 0.5 and takes the other from 0.5 on', () {
      final other = base.copyWith(lang: 'ar', textDirection: TextDirection.rtl);
      expect(base.lerp(other, 0), same(base));
      expect(base.lerp(other, 0.49), same(base));
      expect(base.lerp(other, 0.5), same(other));
      expect(base.lerp(other, 1), same(other));
    });

    test('a null other keeps this theme at every t', () {
      expect(base.lerp(null, 0.9), same(base));
    });
  });

  testWidgets('a rebuild with the same config leaves ThemeData equal', (
    tester,
  ) async {
    late ThemeData first;
    late ThemeData second;
    await tester.pumpWidget(_scope((c) => first = Theme.of(c)));
    await tester.pumpWidget(_scope((c) => second = Theme.of(c)));
    expect(second, first);
    expect(second.extension<HfyTheme>(), base);
  });
}
