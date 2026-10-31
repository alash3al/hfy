// Widget tests of HfyThemeScope: the resolution of theme mode (P-CFG-010),
// language and direction (P-CFG-008, P-CFG-009), density (P-CFG-013), the
// Arabic-family line height (P-VIS-009), font fallback (P-VIS-002) and reduced
// motion (P-VIS-013).

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/theme/theme.dart';
import 'package:hfy_flutter/src/theme/tokens.dart';

Future<void> _pump(
  WidgetTester tester,
  void Function(BuildContext context) seen, {
  Brightness platform = Brightness.light,
  bool reduceMotion = false,
  String? lang,
  TextDirection? dir,
  HfyThemeMode theme = HfyThemeMode.auto,
  HfyDensity density = HfyDensity.comfortable,
}) async {
  await tester.pumpWidget(
    MediaQuery(
      data: MediaQueryData(
        platformBrightness: platform,
        disableAnimations: reduceMotion,
      ),
      child: HfyThemeScope(
        lang: lang,
        dir: dir,
        theme: theme,
        density: density,
        child: Builder(
          builder: (context) {
            seen(context);
            return const SizedBox();
          },
        ),
      ),
    ),
  );
}

// The locale sits above a stable child, so only the Localizations change
// rebuilds the scope.
Future<void> _pumpLocalized(
  WidgetTester tester,
  ValueNotifier<Locale> locale,
  void Function(BuildContext context) seen,
) => tester.pumpWidget(
  MediaQuery(
    data: const MediaQueryData(),
    child: ValueListenableBuilder<Locale>(
      valueListenable: locale,
      builder: (context, value, child) => Localizations(
        locale: value,
        delegates: const [DefaultWidgetsLocalizations.delegate],
        child: child,
      ),
      child: HfyThemeScope(
        child: Builder(
          builder: (context) {
            seen(context);
            return const SizedBox();
          },
        ),
      ),
    ),
  ),
);

void main() {
  group('P-CFG-010 theme mode', () {
    testWidgets('auto follows the platform brightness', (tester) async {
      late HfyTheme t;
      await _pump(tester, (c) => t = HfyTheme.of(c), platform: Brightness.dark);
      expect(t.brightness, Brightness.dark);
      expect(
        t.tokens.bg,
        HfyTheme.resolve(
          platformBrightness: Brightness.dark,
          reduceMotion: false,
        ).tokens.bg,
      );
    });

    testWidgets('an explicit light or dark theme wins over the platform', (
      tester,
    ) async {
      late HfyTheme light;
      await _pump(
        tester,
        (c) => light = HfyTheme.of(c),
        platform: Brightness.dark,
        theme: HfyThemeMode.light,
      );
      expect(light.brightness, Brightness.light);

      late HfyTheme dark;
      await _pump(
        tester,
        (c) => dark = HfyTheme.of(c),
        platform: Brightness.light,
        theme: HfyThemeMode.dark,
      );
      expect(dark.brightness, Brightness.dark);
    });
  });

  group('P-CFG-013 density', () {
    testWidgets(
      'compact changes only the size tokens; comfortable is the default',
      (tester) async {
        late HfyTheme compact;
        await _pump(
          tester,
          (c) => compact = HfyTheme.of(c),
          density: HfyDensity.compact,
        );
        expect(
          [
            compact.tokens.fontSize,
            compact.lineHeight,
            compact.tokens.gap,
            compact.tokens.avatar,
          ],
          [14, 1.42, 8, 26],
        );

        late HfyTheme comfortable;
        await _pump(tester, (c) => comfortable = HfyTheme.of(c));
        expect(
          [
            comfortable.tokens.fontSize,
            comfortable.lineHeight,
            comfortable.tokens.gap,
            comfortable.tokens.avatar,
          ],
          [15, 1.5, 12, 30],
        );
      },
    );
  });

  group('P-CFG-008/009 language and direction', () {
    testWidgets(
      'an Arabic lang is right-to-left in both the theme and Directionality',
      (tester) async {
        late HfyTheme t;
        late TextDirection direction;
        await _pump(tester, (c) {
          t = HfyTheme.of(c);
          direction = Directionality.of(c);
        }, lang: 'ar-EG');
        expect(t.isRtl, isTrue);
        expect(direction, TextDirection.rtl);
      },
    );

    testWidgets('an English lang is left-to-right', (tester) async {
      late TextDirection direction;
      await _pump(tester, (c) => direction = Directionality.of(c), lang: 'en');
      expect(direction, TextDirection.ltr);
    });

    testWidgets('an explicit dir wins over the language', (tester) async {
      late TextDirection direction;
      await _pump(
        tester,
        (c) => direction = Directionality.of(c),
        lang: 'ar',
        dir: TextDirection.ltr,
      );
      expect(direction, TextDirection.ltr);
    });

    testWidgets('without a config lang the device locale decides', (
      tester,
    ) async {
      tester.platformDispatcher.localeTestValue = const Locale('ar');
      addTearDown(tester.platformDispatcher.clearLocaleTestValue);
      late HfyTheme t;
      await _pump(tester, (c) => t = HfyTheme.of(c));
      expect(t.lang, 'ar');
      expect(t.isRtl, isTrue);
    });

    testWidgets('the config lang wins over the device locale', (tester) async {
      tester.platformDispatcher.localeTestValue = const Locale('ar');
      addTearDown(tester.platformDispatcher.clearLocaleTestValue);
      late HfyTheme t;
      await _pump(tester, (c) => t = HfyTheme.of(c), lang: 'en');
      expect(t.lang, 'en');
      expect(t.isRtl, isFalse);
    });

    testWidgets('the app Localizations locale beats the device locale', (
      tester,
    ) async {
      tester.platformDispatcher.localeTestValue = const Locale('ar');
      addTearDown(tester.platformDispatcher.clearLocaleTestValue);
      final locale = ValueNotifier(const Locale('en'));
      addTearDown(locale.dispose);
      late HfyTheme t;
      await _pumpLocalized(tester, locale, (c) => t = HfyTheme.of(c));
      expect(t.lang, 'en');
      expect(t.isRtl, isFalse);
    });

    testWidgets('a Localizations locale change rebuilds the theme: en to ar', (
      tester,
    ) async {
      final locale = ValueNotifier(const Locale('en'));
      addTearDown(locale.dispose);
      late HfyTheme t;
      late TextDirection direction;
      await _pumpLocalized(tester, locale, (c) {
        t = HfyTheme.of(c);
        direction = Directionality.of(c);
      });
      expect([t.lang, t.lineHeight, direction], ['en', 1.5, TextDirection.ltr]);

      locale.value = const Locale('ar');
      await tester.pump();
      expect([t.lang, t.lineHeight, direction], ['ar', 1.7, TextDirection.rtl]);
    });
  });

  group('P-VIS-009 Arabic-family line height', () {
    testWidgets('ar, fa and ur take 1.7 even in compact density', (
      tester,
    ) async {
      for (final lang in ['ar', 'fa', 'ur']) {
        late HfyTheme t;
        await _pump(
          tester,
          (c) => t = HfyTheme.of(c),
          lang: lang,
          density: HfyDensity.compact,
        );
        expect(t.lineHeight, 1.7, reason: lang);
      }
    });
  });

  group('P-VIS-002 fonts and P-VIS-013 motion', () {
    testWidgets(
      'the text style carries the Arabic fallback stack at the density size',
      (tester) async {
        late HfyTheme t;
        await _pump(tester, (c) => t = HfyTheme.of(c));
        final style = t.textStyle;
        expect(style.fontFamily, isNull, reason: 'platform default face');
        expect(
          style.fontFamilyFallback,
          containsAll(['Noto Sans Arabic', 'Segoe UI Arabic', 'Geeza Pro']),
        );
        expect(style.fontSize, 15);
      },
    );

    testWidgets('reduced motion comes from MediaQuery.disableAnimations', (
      tester,
    ) async {
      late HfyTheme t;
      await _pump(tester, (c) => t = HfyTheme.of(c), reduceMotion: true);
      expect(t.reduceMotion, isTrue);
    });
  });
}
