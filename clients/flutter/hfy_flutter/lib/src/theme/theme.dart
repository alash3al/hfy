// The embed's Flutter theme (P-CFG-009..011, P-CFG-013, P-VIS-002, P-VIS-009,
// P-VIS-013): HfyTheme is the ThemeExtension widgets read, resolved once from
// the host config and the platform by HfyThemeScope.

import 'package:flutter/material.dart';

import '../l10n/l10n.dart' show hfyIsRtl, hfyResolveLang;
import 'accent.dart';
import 'tokens.dart';

enum HfyThemeMode { auto, light, dark }

enum HfyLayout { bubbles, transcript }

const hfyDefaultAccent = '#6366f1';

/// ui.css:4 without its generic families: the platform default is the primary
/// face, these cover the glyphs it lacks (Arabic in particular).
const hfyFontFallback = <String>[
  'Segoe UI',
  'Roboto',
  'Helvetica Neue',
  'Noto Sans',
  'Noto Sans Arabic',
  'Segoe UI Arabic',
  'Geeza Pro',
  'Tahoma',
  'Arial',
];

/// ui.css:5 without its generic `monospace` tail.
const hfyMonoFontFallback = <String>[
  'SFMono-Regular',
  'Menlo',
  'Consolas',
  'Liberation Mono',
];

class HfyTheme extends ThemeExtension<HfyTheme> {
  const HfyTheme({
    required this.tokens,
    required this.brightness,
    required this.density,
    required this.layout,
    required this.lang,
    required this.textDirection,
    required this.reduceMotion,
  });

  final HfyTokens tokens;
  final Brightness brightness;
  final HfyDensity density;
  final HfyLayout layout;
  final String lang;
  final TextDirection textDirection;
  final bool reduceMotion;

  bool get isRtl => textDirection == TextDirection.rtl;

  /// `:lang(ar|fa|ur)` takes 1.7 at every density (ui.css:67).
  double get lineHeight =>
      const {'ar', 'fa', 'ur'}.contains(lang.split('-').first)
      ? 1.7
      : tokens.lineHeight;

  TextStyle get textStyle => TextStyle(
    fontSize: tokens.fontSize,
    height: lineHeight,
    color: tokens.text,
    fontFamilyFallback: hfyFontFallback,
  );

  static HfyTheme of(BuildContext context) {
    final theme = Theme.of(context).extension<HfyTheme>();
    if (theme == null) {
      throw StateError('HfyTheme not found: wrap the widget in HfyThemeScope');
    }
    return theme;
  }

  /// Pure resolution of the host config (ui.js:763-771): explicit theme, lang
  /// or dir win over the platform; `dir: null` means auto (RTL from the lang).
  static HfyTheme resolve({
    required Brightness platformBrightness,
    required bool reduceMotion,
    String? lang,
    String? deviceLang,
    TextDirection? dir,
    HfyThemeMode theme = HfyThemeMode.auto,
    String accent = hfyDefaultAccent,
    HfyDensity density = HfyDensity.comfortable,
    HfyLayout layout = HfyLayout.bubbles,
  }) {
    final resolvedLang = hfyResolveLang(
      configLang: lang,
      deviceLang: deviceLang,
    );
    final brightness = switch (theme) {
      HfyThemeMode.light => Brightness.light,
      HfyThemeMode.dark => Brightness.dark,
      HfyThemeMode.auto => platformBrightness,
    };
    return HfyTheme(
      // shortcut: web takes any CSS colour as accent, Dart takes #rgb/#rrggbb only, else the default, upgrade when a host needs named colours.
      tokens: HfyTokens.build(
        accent: HfyColor.parseHex(accent) ?? const HfyColor.hex(0x6366f1),
        brightness: brightness,
        density: density,
      ),
      brightness: brightness,
      density: density,
      layout: layout,
      lang: resolvedLang,
      textDirection:
          dir ??
          (hfyIsRtl(resolvedLang) ? TextDirection.rtl : TextDirection.ltr),
      reduceMotion: reduceMotion,
    );
  }

  @override
  HfyTheme copyWith({
    HfyTokens? tokens,
    Brightness? brightness,
    HfyDensity? density,
    HfyLayout? layout,
    String? lang,
    TextDirection? textDirection,
    bool? reduceMotion,
  }) => HfyTheme(
    tokens: tokens ?? this.tokens,
    brightness: brightness ?? this.brightness,
    density: density ?? this.density,
    layout: layout ?? this.layout,
    lang: lang ?? this.lang,
    textDirection: textDirection ?? this.textDirection,
    reduceMotion: reduceMotion ?? this.reduceMotion,
  );

  /// Switches to [other] at t = 0.5 and keeps this theme before that.
  // shortcut: no colour blending between themes, the switch is instant at t = 0.5; upgrade when a theme change has to animate.
  @override
  HfyTheme lerp(ThemeExtension<HfyTheme>? other, double t) =>
      other is HfyTheme && t >= 0.5 ? other : this;

  /// Value equality. ThemeData compares its extensions with ==, so a rebuild
  /// with the same config leaves the theme unchanged for its dependants.
  @override
  bool operator ==(Object other) =>
      identical(this, other) ||
      other is HfyTheme &&
          tokens == other.tokens &&
          brightness == other.brightness &&
          density == other.density &&
          layout == other.layout &&
          lang == other.lang &&
          textDirection == other.textDirection &&
          reduceMotion == other.reduceMotion;

  @override
  int get hashCode => Object.hash(
    tokens,
    brightness,
    density,
    layout,
    lang,
    textDirection,
    reduceMotion,
  );
}

/// Resolves [HfyTheme] from the host config and the platform, and provides it
/// with the matching [Directionality] (the root wiring of P-CFG-011).
class HfyThemeScope extends StatelessWidget {
  const HfyThemeScope({
    super.key,
    required this.child,
    this.lang,
    this.dir,
    this.theme = HfyThemeMode.auto,
    this.accent = hfyDefaultAccent,
    this.density = HfyDensity.comfortable,
    this.layout = HfyLayout.bubbles,
  });

  final Widget child;
  final String? lang;
  final TextDirection? dir;
  final HfyThemeMode theme;
  final String accent;
  final HfyDensity density;
  final HfyLayout layout;

  @override
  Widget build(BuildContext context) {
    // Localizations registers a dependency, so an app locale change rebuilds
    // this scope. With no Localizations above, the view's platform locale is
    // read at build time, so a bare platform change shows on the next rebuild.
    final locale =
        Localizations.maybeLocaleOf(context) ??
        View.of(context).platformDispatcher.locale;
    final resolved = HfyTheme.resolve(
      platformBrightness: MediaQuery.platformBrightnessOf(context),
      reduceMotion: MediaQuery.disableAnimationsOf(context),
      lang: lang,
      deviceLang: locale.toLanguageTag(),
      dir: dir,
      theme: theme,
      accent: accent,
      density: density,
      layout: layout,
    );
    final base = Theme.of(context);
    return Theme(
      data: base.copyWith(extensions: [...base.extensions.values, resolved]),
      child: Directionality(
        textDirection: resolved.textDirection,
        child: child,
      ),
    );
  }
}
