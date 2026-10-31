// Purity guard for the shareable core (PARITY.md Phase 1-2): the model, the SSE
// framing, the dedupe, the colour math and the string tables must not import
// dart:io, dart:ui or package:flutter, so they can compile to JS later.

import 'dart:io' show Directory, File;

import 'package:flutter_test/flutter_test.dart';

const _pureFiles = [
  'lib/src/client/sse.dart',
  'lib/src/client/dedupe.dart',
  'lib/src/theme/accent.dart',
  'lib/src/l10n/strings.dart',
  'lib/src/l10n/l10n.dart',
];

final _import = RegExp(
  r'''^\s*(?:import|export)\s+['"]([^'"]+)['"]''',
  multiLine: true,
);

bool _banned(String uri) =>
    uri == 'dart:io' ||
    uri == 'dart:ui' ||
    uri == 'package:flutter' ||
    uri.startsWith('package:flutter/');

List<String> _bannedImports(String source) => [
  for (final m in _import.allMatches(source))
    if (_banned(m.group(1)!)) m.group(1)!,
];

void main() {
  final files = [
    for (final f in Directory('lib/src/model').listSync(recursive: true))
      if (f is File && f.path.endsWith('.dart')) f.path,
    for (final path in _pureFiles) path,
  ];

  test(
    'the guard covers the model, the SSE and dedupe, the colour math and the strings',
    () {
      expect(files, containsAll(_pureFiles));
      expect(files.where((p) => p.startsWith('lib/src/model/')), isNotEmpty);
      for (final path in files) {
        expect(File(path).existsSync(), isTrue, reason: path);
      }
    },
  );

  test('no shareable file imports dart:io, dart:ui or package:flutter', () {
    for (final path in files) {
      expect(
        _bannedImports(File(path).readAsStringSync()),
        isEmpty,
        reason: path,
      );
    }
  });

  test('the matcher catches each banned import form and ignores comments', () {
    expect(_bannedImports("import 'dart:io';"), ['dart:io']);
    expect(_bannedImports('export "dart:ui" show Color;'), ['dart:ui']);
    expect(_bannedImports("import 'package:flutter/material.dart';"), [
      'package:flutter/material.dart',
    ]);
    expect(
      _bannedImports(
        "import 'package:flutter/foundation.dart' show ValueNotifier;",
      ),
      ['package:flutter/foundation.dart'],
    );
    expect(_bannedImports("// import 'dart:io';"), isEmpty);
    expect(_bannedImports("import 'dart:convert' show jsonDecode;"), isEmpty);
  });
}
