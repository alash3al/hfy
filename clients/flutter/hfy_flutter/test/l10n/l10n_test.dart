// Localisation behaviour (P-CFG-007/008, P-L10N-002..007, P-L10N-010): string
// lookup and placeholders, RTL detection, language resolution, and the date,
// time, day, size and initials formats. Expected strings were produced by node's
// Intl (en-US and ar, zone UTC) and by ui.js's own functions, then copied here.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/l10n/l10n.dart';
import 'package:hfy_flutter/src/model/state.dart' show HfyTz;

int _ms(int y, int mo, int d, [int h = 0, int mi = 0]) =>
    DateTime.utc(y, mo, d, h, mi).millisecondsSinceEpoch;

final _oct31 = _ms(2026, 10, 31, 14, 5);
final _jan2 = _ms(2025, 1, 2, 0, 7);
final _jun9 = _ms(2026, 6, 9, 12, 30);
final _dec25 = _ms(2026, 12, 25, 23, 59);
final _sep3 = _ms(2026, 9, 3, 9);
const _day = 86400000;

HfyL10n _en() => const HfyL10n(lang: 'en', tz: HfyTz.utc());
HfyL10n _ar() => const HfyL10n(lang: 'ar', tz: HfyTz.utc());

void main() {
  group('P-L10N-002 placeholders', () {
    test('{n} and {total} fill the counter strings in each language', () {
      expect(_en().t('stepOf', {'n': 2, 'total': 5}), '2 of 5');
      expect(_ar().t('stepOf', {'n': 2, 'total': 5}), '2 من 5');
    });

    test('{size} fills the size error', () {
      expect(
        _en().t('errSize', {'size': '10 MB'}),
        'Files must be at most 10 MB',
      );
    });

    test(
      'only the first occurrence of a placeholder is replaced, as String.replace does',
      () {
        final l10n = HfyL10n(lang: 'en', custom: const {'stepOf': '{n}/{n}'});
        expect(l10n.t('stepOf', {'n': 1, 'total': 2}), '1/{n}');
      },
    );

    test('values are inserted literally', () {
      expect(_en().t('waitingFor', {'name': r'$&'}), r'Waiting for $&');
    });
  });

  group('P-CFG-007 string precedence', () {
    test('custom, then the language table, then en, then the key', () {
      expect(_en().t('send'), 'Send');
      expect(_ar().t('send'), 'إرسال');
      expect(const HfyL10n(lang: 'ar-SA').t('send'), 'إرسال');
      expect(const HfyL10n(lang: 'fr').t('send'), 'Send');
      expect(const HfyL10n(lang: 'en').t('no-such-key'), 'no-such-key');
      expect(const HfyL10n(lang: 'ar', custom: {'send': 'Go'}).t('send'), 'Go');
    });
  });

  group('P-CFG-008 language resolution', () {
    test('config lang, then device locale, then en', () {
      expect(hfyResolveLang(), 'en');
      expect(hfyResolveLang(configLang: '', deviceLang: 'ar'), 'ar');
      expect(hfyResolveLang(configLang: 'fr', deviceLang: 'ar'), 'fr');
    });
  });

  group('P-L10N-003 RTL script list', () {
    test('ar he fa ur dv ps sd yi are right-to-left by first subtag', () {
      for (final lang in [
        'ar',
        'he',
        'fa',
        'ur',
        'dv',
        'ps',
        'sd',
        'yi',
        'ar-EG',
        'fa-IR',
      ]) {
        expect(HfyL10n(lang: lang).isRtl, isTrue, reason: lang);
      }
    });

    test(
      'other languages are left-to-right, and the match is case-sensitive like ui.js',
      () {
        for (final lang in ['en', 'fr', 'ja', 'en-US', 'AR']) {
          expect(HfyL10n(lang: lang).isRtl, isFalse, reason: lang);
        }
      },
    );
  });

  group('P-L10N-004 time, full date and day (Intl en and ar shapes)', () {
    test(
      'time is the hour and 2-digit minute, with the AM/PM or ص/م marker',
      () {
        expect(_en().time(_oct31), '2:05 PM');
        expect(_en().time(_jan2), '12:07 AM');
        expect(_en().time(_jun9), '12:30 PM');
        expect(_en().time(_dec25), '11:59 PM');
        expect(_ar().time(_oct31), '2:05 م');
        expect(_ar().time(_jan2), '12:07 ص');
        expect(_ar().time(_sep3), '9:00 ص');
      },
    );

    test(
      'full is the medium date with the short time (the title attribute)',
      () {
        expect(_en().full(_oct31), 'Oct 31, 2026, 2:05 PM');
        expect(_en().full(_jan2), 'Jan 2, 2025, 12:07 AM');
        expect(_en().full(_dec25), 'Dec 25, 2026, 11:59 PM');
        expect(_ar().full(_oct31), '31\u200f/10\u200f/2026\u060c 2:05 م');
        expect(_ar().full(_jan2), '02\u200f/01\u200f/2025\u060c 12:07 ص');
        expect(_ar().full(_sep3), '03\u200f/09\u200f/2026\u060c 9:00 ص');
      },
    );

    test(
      'day names the weekday, month and day, and adds the year only for another year',
      () {
        expect(_en().day(_oct31, _oct31), 'Saturday, October 31');
        expect(_en().day(_jun9, _oct31), 'Tuesday, June 9');
        expect(_en().day(_jan2, _oct31), 'Thursday, January 2, 2025');
        expect(_ar().day(_oct31, _oct31), 'السبت، 31 أكتوبر');
        expect(_ar().day(_jun9, _oct31), 'الثلاثاء، 9 يونيو');
        expect(_ar().day(_jan2, _oct31), 'الخميس، 2 يناير 2025');
      },
    );

    test('times follow the zone, not the device (P-CFG-023)', () {
      const plus3 = HfyL10n(
        lang: 'en',
        tz: HfyTz.fixed('UTC+3', Duration(hours: 3)),
      );
      expect(plus3.time(_ms(2026, 10, 31, 22, 5)), '1:05 AM');
    });
  });

  group('P-L10N-005 day labels', () {
    test(
      'today and yesterday compare calendar day keys, not 24-hour spans',
      () {
        expect(_en().dayLabel(_oct31, _oct31 + 2 * 3600000), 'Today');
        expect(_en().dayLabel(_jan2, _jan2 + _day), 'Yesterday');
        expect(
          _en().dayLabel(_jan2, _ms(2026, 10, 31)),
          'Thursday, January 2, 2025',
        );
      },
    );

    test('the same labels come from the ar table', () {
      expect(_ar().dayLabel(_oct31, _oct31 + 2 * 3600000), 'اليوم');
      expect(_ar().dayLabel(_jan2, _jan2 + _day), 'أمس');
    });
  });

  group('P-L10N-006 hfySize (Intl unit style, 1024 steps)', () {
    test(
      'en: bytes are narrow and whole, larger units are short with at most one decimal',
      () {
        final cases = <num, String>{
          0: '0B',
          1: '1B',
          13: '13B',
          999: '999B',
          1000: '1,000B',
          1023: '1,023B',
          1024: '1 kB',
          1025: '1 kB',
          1536: '1.5 kB',
          10 * 1024 * 1024: '10 MB',
          1.5 * 1024 * 1024: '1.5 MB',
          1048576: '1 MB',
          5 * 1024 * 1024 * 1024: '5 GB',
          1073741824: '1 GB',
          1000000000: '953.7 MB',
          2 * 1024 * 1024 * 1024 * 1024: '2,048 GB',
          12345678: '11.8 MB',
          0.5: '1B',
          2047.9 * 1024: '2 MB',
        };
        cases.forEach((n, want) {
          expect(hfySize(n, 'en'), want, reason: '$n bytes');
        });
      },
    );

    test(
      'ar: Arabic unit words with ASCII digits, as node Intl prints the ar tag',
      () {
        expect(hfySize(0, 'ar'), '0 ب');
        expect(hfySize(999, 'ar'), '999 ب');
        expect(hfySize(1023, 'ar'), '1,023 ب');
        expect(hfySize(1024, 'ar'), '1 كيلوبايت');
        expect(hfySize(1536, 'ar'), '1.5 كيلوبايت');
        expect(hfySize(1048576, 'ar'), '1 م.ب');
        expect(hfySize(10 * 1024 * 1024, 'ar'), '10 م.ب');
        expect(hfySize(1000000000, 'ar'), '953.7 م.ب');
        expect(hfySize(1073741824, 'ar'), '1 غ.ب');
        expect(hfySize(5 * 1024 * 1024 * 1024, 'ar'), '5 غ.ب');
        expect(hfySize(2 * 1024 * 1024 * 1024 * 1024, 'ar'), '2,048 غ.ب');
      },
    );

    test('invalid sizes give an empty string', () {
      expect(hfySize(-1, 'en'), '');
      expect(hfySize(double.nan, 'en'), '');
    });

    test('other languages take the en shape (shortcut)', () {
      expect(hfySize(1536, 'fr'), '1.5 kB');
    });
  });

  group('P-L10N-007 initials', () {
    test('first code point of the first two parts, uppercased', () {
      final cases = {
        'Ada Lovelace': 'AL',
        'jean-luc.picard': 'JL',
        '  solo  ': 'S',
        'ana@example.com': 'AE',
        '': '',
        'ølga kristiansen': 'ØK',
        '😀 smile': '😀S',
        'a b c': 'AB',
        'x_y': 'XY',
        'Ada  \t Lovelace': 'AL',
        'ß straße': 'ßS', // shortcut: JS would give SSS, see hfyInitials
      };
      cases.forEach((name, want) {
        expect(hfyInitials(name), want, reason: name);
      });
    });
  });

  group('P-L10N-010 title', () {
    test('the document title is the title string', () {
      expect(_en().t('title'), 'Conversation');
      expect(_ar().t('title'), 'المحادثة');
    });
  });
}
