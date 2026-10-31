// Language and direction rules of the embed (ui.js:32, 763-767). Pure Dart:
// no Flutter and no dart:io, so the same rules can run in the JS-compiled core.

const _rtlLanguages = {'ar', 'he', 'fa', 'ur', 'dv', 'ps', 'sd', 'yi'};

/// ui.js:764-767: the first subtag decides right-to-left (case-sensitive, like
/// the JS list).
bool hfyIsRtl(String lang) => _rtlLanguages.contains(lang.split('-').first);

/// ui.js:763 `config.lang || navigator.language || 'en'`.
String hfyResolveLang({String? configLang, String? deviceLang}) {
  if (configLang != null && configLang.isNotEmpty) return configLang;
  if (deviceLang != null && deviceLang.isNotEmpty) return deviceLang;
  return 'en';
}
