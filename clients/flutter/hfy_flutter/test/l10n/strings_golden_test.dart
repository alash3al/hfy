// Golden parity of the string tables (P-L10N-001, P-L10N-012): the en and ar
// tables must equal clients/fixtures/strings.json exactly, with the same keys.

import 'dart:convert' show jsonDecode;
import 'dart:io' show File;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/l10n/strings.dart';

import '../helpers/fixture_loader.dart';

void main() {
  final golden =
      jsonDecode(File('${fixturesDir().path}/strings.json').readAsStringSync())
          as Map<String, dynamic>;

  test('P-L10N-001 the en table equals strings.json en exactly (86 keys)', () {
    expect(hfyStringsEn, hasLength(86));
    expect(hfyStringsEn, equals(golden['en']));
  });

  test('P-L10N-001 the ar table equals strings.json ar exactly (86 keys)', () {
    expect(hfyStringsAr, hasLength(86));
    expect(hfyStringsAr, equals(golden['ar']));
  });

  test('P-L10N-012 en and ar have identical key sets', () {
    expect(hfyStringsAr.keys.toSet(), hfyStringsEn.keys.toSet());
  });

  test('P-L10N-001 the table registry resolves both languages', () {
    expect(hfyStringTables.keys, unorderedEquals(['en', 'ar']));
    expect(hfyStringTables['ar'], same(hfyStringsAr));
  });
}
