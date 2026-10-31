// Language rules, string lookup and formatters of the embed (ui.js:12-32,
// 124-145, 757-771, 1052-1064). Pure Dart: no Flutter, no dart:io, no ICU.
//
// The date, time and size shapes are those Intl produces for en and ar (the
// two languages the tables ship); every other language takes the en shape.
// shortcut: non-en/ar languages use English month, weekday and unit words, upgrade with an ICU-backed formatter when hosts need more locales.

import '../model/state.dart' show HfyTz;
import 'strings.dart';

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

/// ui.js hfyInitials (142-145): the first code point of the first two name parts.
// shortcut: Dart's toUpperCase skips special casings (ß stays ß, JS gives SS), upgrade when a name needs them.
String hfyInitials(String name) => name
    .trim()
    .split(_namePart)
    .where((part) => part.isNotEmpty)
    .take(2)
    .map((part) => String.fromCharCodes([part.runes.first]).toUpperCase())
    .join();

final _namePart = RegExp(r'[\s._@-]+');

/// ui.js hfySize (133-141): binary steps, Intl unit words, and thousands
/// grouping. Bytes are whole numbers, larger units keep at most one decimal.
String hfySize(num n, String lang) {
  if (!(n >= 0)) return '';
  var x = n.toDouble();
  var i = 0;
  while (x >= 1024 && i < 3) {
    x /= 1024;
    i++;
  }
  final number = _number(x, i == 0 ? 0 : 1);
  if (_isAr(lang)) return '$number ${_arUnits[i]}';
  return i == 0 ? '$number${_enUnits[i]}' : '$number ${_enUnits[i]}';
}

const _enUnits = ['B', 'kB', 'MB', 'GB'];
const _arUnits = ['ب', 'كيلوبايت', 'م.ب', 'ج.ب'];

const _enMonths = [
  'January',
  'February',
  'March',
  'April',
  'May',
  'June',
  'July',
  'August',
  'September',
  'October',
  'November',
  'December',
];
const _enShortMonths = [
  'Jan',
  'Feb',
  'Mar',
  'Apr',
  'May',
  'Jun',
  'Jul',
  'Aug',
  'Sep',
  'Oct',
  'Nov',
  'Dec',
];
const _arMonths = [
  'يناير',
  'فبراير',
  'مارس',
  'أبريل',
  'مايو',
  'يونيو',
  'يوليو',
  'أغسطس',
  'سبتمبر',
  'أكتوبر',
  'نوفمبر',
  'ديسمبر',
];
const _enWeekdays = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
];
const _arWeekdays = [
  'الأحد',
  'الاثنين',
  'الثلاثاء',
  'الأربعاء',
  'الخميس',
  'الجمعة',
  'السبت',
];

final _thousands = RegExp(r'\B(?=(\d{3})+(?!\d))');

String _number(double x, int maxFraction) {
  var text = x.toStringAsFixed(maxFraction);
  if (maxFraction > 0) {
    text = text.replaceFirst(RegExp(r'\.?0+$'), '');
  }
  final dot = text.indexOf('.');
  final whole = dot < 0 ? text : text.substring(0, dot);
  final fraction = dot < 0 ? '' : text.substring(dot);
  return '${whole.replaceAllMapped(_thousands, (_) => ',')}$fraction';
}

bool _isAr(String lang) => lang.split('-').first == 'ar';

String _two(int n) => n.toString().padLeft(2, '0');

class HfyL10n {
  const HfyL10n({
    required this.lang,
    this.custom = const {},
    this.tz = const HfyTz.local(),
  });

  /// The resolved language tag (see [hfyResolveLang]), e.g. `ar-EG`.
  final String lang;

  /// Host strings, checked before the tables (P-CFG-007, P-CFG-025).
  final Map<String, String> custom;

  /// The zone that times and day keys use (P-CFG-023).
  final HfyTz tz;

  bool get isRtl => hfyIsRtl(lang);

  bool get _ar => _isAr(lang);

  /// ui.js `t()` (757-762): custom, then the language table, then en, then the
  /// key. Each `{name}` placeholder is replaced once, at its first occurrence,
  /// as `String.replace` does in ui.js.
  // shortcut: `$&`-style patterns in values stay literal (ui.js expands them), keep unless a host relies on that quirk.
  String t(String key, [Map<String, Object> args = const {}]) {
    var s =
        custom[key] ??
        hfyStringTables[lang.split('-').first]?[key] ??
        hfyStringsEn[key] ??
        key;
    args.forEach((name, value) {
      s = s.replaceFirst('{$name}', '$value');
    });
    return s;
  }

  /// ui.js fmtTime: the hour and a 2-digit minute, in [tz].
  String time(int ms) {
    final d = _wall(ms);
    final pm = d.hour >= 12;
    final hour = d.hour % 12 == 0 ? 12 : d.hour % 12;
    final marker = _ar ? (pm ? 'م' : 'ص') : (pm ? 'PM' : 'AM');
    return '$hour:${_two(d.minute)} $marker';
  }

  /// ui.js fullFmt (medium date, short time): the message title attribute.
  String full(int ms) => _ar
      ? '${_date(_wall(ms))}\u060c ${time(ms)}'
      : '${_date(_wall(ms))}, ${time(ms)}';

  /// ui.js fmtDay: weekday, month and day, plus the year when it differs from
  /// the year of [nowMs] (device time, as ui.js reads `getFullYear`).
  String day(int ms, int nowMs) {
    final d = _wall(ms);
    final yearShown =
        DateTime.fromMillisecondsSinceEpoch(ms).year !=
        DateTime.fromMillisecondsSinceEpoch(nowMs).year;
    if (_ar) {
      final year = yearShown ? ' ${d.year}' : '';
      return '${_arWeekdays[d.weekday % 7]}\u060c ${d.day} ${_arMonths[d.month - 1]}$year';
    }
    final year = yearShown ? ', ${d.year}' : '';
    return '${_enWeekdays[d.weekday % 7]}, ${_enMonths[d.month - 1]} ${d.day}$year';
  }

  /// ui.js dayText (1144): Today and Yesterday by calendar day key in [tz],
  /// otherwise [day].
  String dayLabel(int ms, int nowMs) {
    final key = tz.dayKey(ms);
    if (key == tz.dayKey(nowMs)) return t('today');
    final yesterday = tz.dayKey(nowMs - Duration.millisecondsPerDay);
    if (key == yesterday) return t('yesterday');
    return day(ms, nowMs);
  }

  String _date(DateTime d) => _ar
      ? '${_two(d.day)}\u200f/${_two(d.month)}\u200f/${d.year}'
      : '${_enShortMonths[d.month - 1]} ${d.day}, ${d.year}';

  DateTime _wall(int ms) => tz.device
      ? DateTime.fromMillisecondsSinceEpoch(ms)
      : DateTime.fromMillisecondsSinceEpoch(
          ms + tz.offset.inMilliseconds,
          isUtc: true,
        );
}
