// The markdown fixture contract (clients/fixtures/markdown.json, PARITY P-MD-001
// and every P-MD rule the 105 hand-authored cases pin): for EACH case the
// input's sha256 matches the stored hash, and `hfyMdParse(input)` serialized
// with toJson deep-equals the tree the real ui.js produced. A mismatch names
// the case and the first differing JSON path.

import 'dart:convert' show jsonDecode, utf8;
import 'dart:io' show File;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/parser.dart';

import '../helpers/fixture_loader.dart' show fixturesDir;
import '../support/projection.dart' show firstJsonDifference;
import 'support/sha256.dart';

List<Map<String, dynamic>> _markdownCases() {
  final file = File('${fixturesDir().path}/markdown.json');
  final value = jsonDecode(file.readAsStringSync());
  if (value is! List) {
    throw StateError('markdown.json must be a JSON array of cases');
  }
  return [for (final c in value) c as Map<String, dynamic>];
}

void main() {
  final cases = _markdownCases();

  test('P-MD golden: the fixture holds all 105 committed cases', () {
    expect(cases, hasLength(105));
  });

  for (final c in cases) {
    final name = c['name'] as String;
    test('P-MD golden: $name', () {
      final input = c['input'] as String;
      final hash = sha256Hex(utf8.encode(input));
      expect(
        hash,
        c['sha256'],
        reason: 'case $name: input was edited without regenerating the fixture',
      );
      final got = [for (final b in hfyMdParse(input)) b.toJson()];
      final diff = firstJsonDifference(got, c['tree']);
      expect(
        diff,
        isNull,
        reason: 'case $name differs from the ui.js tree at JSON path $diff',
      );
    });
  }
}
