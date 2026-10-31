// The shared fixture contract (clients/fixtures): for EVERY events/*.json
// sequence the Dart fold (lib/src/model/fold.dart) must produce the same
// projection the real JS fold produces — exactly the bytes of
// expected/<name>.json up to object-key order (clients/fixtures/README.md:
// "object keys are sorted at every level", arrays stay exact).
//
// The driving logic mirrors fixtures_test.go's JS harness (history leading
// run, liveAfter pinning, per-call loadOlder pages, duplicate-delivery
// through the real HfyDedupe, end-of-scenario relayout/sync/minimize) — see
// test/support/fixture_harness.dart.

import 'dart:convert' show jsonDecode, jsonEncode;
import 'dart:io' show Directory, File, Platform;

import 'package:flutter_test/flutter_test.dart';

import 'helpers/fixture_loader.dart';
import 'support/fixture_harness.dart' show runScenario;
import 'support/projection.dart' show firstJsonDifference;

Set<String> _namesIn(String sub) {
  final dir = Directory('${fixturesDir().path}${_sep()}$sub');
  return {
    for (final e in dir.listSync())
      if (e is File && e.path.endsWith('.json'))
        _base(e.path.split(_sep()).last),
  };
}

String _sep() => Platform.pathSeparator;

String _base(String path) => path.substring(0, path.length - '.json'.length);

Map<String, dynamic> _expectedOf(String name) {
  final file = File(
    '${fixturesDir().path}${_sep()}expected${_sep()}$name.json',
  );
  final value = jsonDecode(file.readAsStringSync());
  if (value is! Map<String, dynamic>) {
    throw StateError('expected file $name.json must be a JSON object');
  }
  return value;
}

void main() {
  test('every events fixture has an expected file and vice versa', () {
    final events = _namesIn('events');
    final expected = _namesIn('expected');
    final missing = {
      for (final n in events)
        if (!expected.contains(n)) n,
    };
    final extra = {
      for (final n in expected)
        if (!events.contains(n)) n,
    };
    expect(missing, isEmpty, reason: 'events without expected/<name>.json');
    expect(extra, isEmpty, reason: 'expected files without events/<name>.json');
  });

  final cases = fixtureFiles();
  test('fixture discovery finds the monorepo fixtures', () {
    expect(
      cases,
      isNotEmpty,
      reason: 'no event fixtures under clients/fixtures/events',
    );
  });
  for (final def in cases) {
    final name = def['name'] as String?;
    test('fold projection matches expected/$name.json', () {
      expect(name, isNotNull, reason: 'fixture header name missing');
      final expected = _expectedOf(name!);
      final got = runScenario(def);
      final diff = firstJsonDifference(got, expected);
      if (diff != null) {
        // Print both shapes small: the scenario name and the first differing
        // JSON path, per the task contract.
        fail(
          'scenario $name differs at the JSON path $diff\n'
          'got:  ${_clip(jsonEncode(got))}\n'
          'want: ${_clip(jsonEncode(expected))}',
        );
      }
    });
  }
}

String _clip(String s, [int n = 3000]) =>
    s.length <= n ? s : '${s.substring(0, n)}…';
