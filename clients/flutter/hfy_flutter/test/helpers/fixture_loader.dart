// Locates the monorepo's clients/fixtures directory from any test's cwd.
import 'dart:convert' show jsonDecode;
import 'dart:io';

/// Walks up from the current directory until a `clients/fixtures/events`
/// directory exists, and returns its parent (`clients/fixtures`).
Directory fixturesDir() {
  var dir = Directory.current;
  for (;;) {
    final candidate = Directory(_p(dir.path, 'clients/fixtures'));
    if (candidate.existsSync()) return candidate;
    final parent = dir.parent;
    if (parent.path == dir.path) {
      throw StateError('clients/fixtures not found from ${Directory.current.path}');
    }
    dir = parent;
  }
}

List<Map<String, dynamic>> fixtureFiles() {
  final files = <Map<String, dynamic>>[];
  for (final path in _sortedJson(fixturesDir(), 'events')) {
    files.add(jsonAt(path));
  }
  return files;
}

Map<String, dynamic> fixtureFile(String name) =>
    jsonAt(_p(fixturesDir().path, 'events/$name.json'));

String _p(String base, String rest) {
  final scrubbed =
      base.endsWith(Platform.pathSeparator) ? base.substring(0, base.length - 1) : base;
  return '$scrubbed${Platform.pathSeparator}$rest';
}

List<String> _sortedJson(Directory dir, String sub) {
  final target = Directory(_p(dir.path, sub));
  return [
    for (final e in target.listSync(recursive: false))
      if (e is File && e.path.endsWith('.json')) e.path,
  ]..sort();
}

Map<String, dynamic> jsonAt(String path) {
  final value = jsonDecode(File(path).readAsStringSync());
  if (value is! Map<String, dynamic>) {
    throw StateError('fixture file $path must be a JSON object');
  }
  return value;
}
