// Documented divergences (PARITY P-MD-011): links the policy refuses on purpose,
// each with the answer of the real ui.js (documented_divergences.json, written by
// tool/regen_js_cases.js). The policy answer is always the fail-closed one: the
// text stays plain, with no link.
//
// The browser differs. The old port linked all eleven attack hosts to xn--
// labels. The browser refuses five of them (U+202E, ZWJ, ZWNJ, the mixed-digit
// host, the tag characters: the policy refuses them too) and links six
// elsewhere (U+2460 -> 1.com, U+1D41A -> a.example, U+00B9 -> 1.example,
// g+U+0307 -> xn--vea.example, U+3164 and U+115F -> an empty label). Every
// non-ASCII host is refused, so the browser's xn-- links are the divergence.
// A mailto path keeps its raw space in the browser; the policy refuses it.
// Eight xn-- labels whose decoded text the browser refuses (uppercase, the
// Kelvin sign, a compatibility ligature, a leading mark, an enclosed digit,
// the dot variants) are refused here too. Two more still link (NFC and the
// mixed-digit bidi label), pinned as known leftovers in url_test.dart.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/links.dart';

import 'support/data.dart';

void main() {
  final rows = markdownDataCases('documented_divergences.json');

  test('P-MD-011: the divergence table holds its 33 rows', () {
    expect(rows, hasLength(33));
    expect(rows.where((r) => r['group'] == 'attack'), hasLength(11));
    expect(rows.where((r) => r['group'] == 'xn-label'), hasLength(8));
  });

  for (final row in rows) {
    final group = row['group'] as String;
    final input = row['input'] as String;
    final browser = row['browser'] as String?;
    final gap = browser == null
        ? 'the browser refuses it too'
        : 'the browser gives $browser';
    final name =
        'P-MD-011 divergence $group ${_show(input)}: fail closed, $gap';
    if (row['kind'] == 'href') {
      test(name, () => expect(hfySafeHref(input), isNull));
    } else {
      final base = Uri.parse(row['base'] as String);
      test(name, () => expect(hfySafeUrl(input, base), ''));
    }
  }
}

String _show(String s) => s.runes
    .map(
      (c) => c < 0x20 || c > 0x7E
          ? 'U+${c.toRadixString(16).toUpperCase()}'
          : String.fromCharCode(c),
    )
    .join();
