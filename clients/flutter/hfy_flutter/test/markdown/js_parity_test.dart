// Parser parity beyond the 105 fixture cases (PARITY P-MD-002..012, P-MD-026):
// js_parity_cases.json holds inputs chosen for the rules the Dart port
// implements differently in form (linear-time rewrites, memoised emphasis
// closers, the maximal-run item regex) and the edge rules around them. Every
// tree there was produced by the real ui.js (tool/regen_js_cases.js).

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/parser.dart';

import '../support/projection.dart' show firstJsonDifference;
import 'support/data.dart';

void main() {
  final cases = markdownDataCases('js_parity_cases.json');

  test('P-MD parity: the oracle file holds its 106 cases', () {
    expect(cases, hasLength(106));
  });

  for (final c in cases) {
    final name = c['name'] as String;
    test('P-MD parity: $name', () {
      final got = [
        for (final b in hfyMdParse(c['input'] as String)) b.toJson(),
      ];
      final diff = firstJsonDifference(got, c['tree']);
      expect(
        diff,
        isNull,
        reason: 'case $name differs from the ui.js tree at JSON path $diff',
      );
    });
  }
}
