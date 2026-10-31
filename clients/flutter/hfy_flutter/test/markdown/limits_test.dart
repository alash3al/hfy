// The bounds the Dart port adds to ui.js (documented deviations; ui.js has no
// such limits): the input cap (hfyMdMaxInput, plain paragraphs past it), the
// 2048-character cap on a bare URL, and the memory the parser holds at the cap.
// The isolate threshold is the widget phase's cue to parse off the main
// isolate.

import 'dart:io' show ProcessInfo;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/nodes.dart';
import 'package:hfy_flutter/src/markdown/parser.dart';

const int _mib = 1024 * 1024;

// The cap as specified (256 KiB). The behaviour tests use this literal, not the
// constant, so a changed constant fails them instead of resizing their inputs.
const int _cap = 262144;

String _repeat(String s, int times) => List.filled(times, s).join();

void main() {
  test('P-MD bounds: the cap is 256 KiB and the isolate cue sits below it', () {
    expect(hfyMdMaxInput, 262144);
    expect(hfyMdIsolateThreshold, 32768);
    expect(hfyMdIsolateThreshold, lessThan(hfyMdMaxInput));
  });

  test('P-MD bounds: an input of exactly the cap is parsed in full', () {
    final input = '*a*${_repeat(' ', _cap - 3)}';
    expect(input.length, _cap);
    final p = hfyMdParse(input).single as HfyMdParagraph;
    expect(p.children.first, isA<HfyMdEmphasis>());
  });

  test('P-MD bounds: past the cap, text is plain paragraphs, not parsed', () {
    final first = '*emphasis* and [link](https://x.example)';
    final input = '$first\r\n# not a heading\n\n${_repeat('a', _cap)}';
    expect(input.length, greaterThan(_cap));
    final tree = hfyMdParse(input);
    expect(tree, hasLength(2));
    expect(tree[0].toJson(), {
      't': 'p',
      'c': [
        {'t': 'text', 'v': '$first\n# not a heading'},
      ],
    });
    expect(tree[1].toJson(), {
      't': 'p',
      'c': [
        {'t': 'text', 'v': _repeat('a', _cap)},
      ],
    });
  });

  test('P-MD bounds: a 2000-level nested list past the cap is plain text', () {
    final lines = [for (var k = 0; k < 2000; k++) '${_repeat(' ', 2 * k)}- x'];
    final tree = hfyMdParse(lines.join('\n'));
    expect(tree.every((b) => b is HfyMdParagraph), isTrue);
  });

  test(
    'P-MD bounds: a 5 MB input parses in under a second, under 100 MB growth',
    () {
      // One paragraph: uncapped, its emphasis memo and tree would be large.
      final unit = '*a _b [x](y) https://x.example/ **c** `d` ';
      final input = _repeat(unit, 5 * _mib ~/ unit.length);
      final before = ProcessInfo.currentRss;
      final watch = Stopwatch()..start();
      final tree = hfyMdParse(input);
      watch.stop();
      final growth = ProcessInfo.currentRss - before;
      expect(
        watch.elapsedMilliseconds,
        lessThan(1000),
        reason: '${input.length} chars',
      );
      expect(
        growth,
        lessThan(100 * _mib),
        reason: 'RSS grew ${growth ~/ _mib} MiB',
      );
      expect(tree.single.toJson(), {
        't': 'p',
        'c': [
          {'t': 'text', 'v': input.trimRight()},
        ],
      });
    },
  );

  test('P-MD bounds: the worst memo at the cap stays far below 100 MB', () {
    // Every character is a delimiter, so every position owns a memo row: the
    // worst case per string. The memo is about 23 MiB (84 bytes per
    // delimiter); the RSS delta also holds the output tree and GC headroom.
    final input = _repeat('*_', _cap ~/ 2);
    final before = ProcessInfo.currentRss;
    final watch = Stopwatch()..start();
    hfyMdParse(input);
    watch.stop();
    final growth = ProcessInfo.currentRss - before;
    expect(watch.elapsedMilliseconds, lessThan(5000));
    expect(
      growth,
      lessThan(100 * _mib),
      reason: 'RSS grew ${growth ~/ _mib} MiB',
    );
  });

  test('P-MD-012 a bare URL with 2048 characters after the scheme links', () {
    final url = 'https://x.example/${_repeat('a', 2048 - 'x.example/'.length)}';
    expect(url.length, 'https://'.length + 2048);
    final p = hfyMdParse('see $url').single as HfyMdParagraph;
    final links = p.children.whereType<HfyMdLink>().toList();
    expect(links, hasLength(1));
    expect(links.single.href, url);
  });

  test('P-MD-012 a bare URL longer than 2048 characters stays plain text', () {
    // Documented deviation: ui.js links it whole. The cap bounds the scan.
    final url = 'https://x.example/${_repeat('a', 2049 - 'x.example/'.length)}';
    final p = hfyMdParse('see $url').single as HfyMdParagraph;
    expect(p.children.whereType<HfyMdLink>(), isEmpty);
    expect(p.toJson(), {
      't': 'p',
      'c': [
        {'t': 'text', 'v': 'see $url'},
      ],
    });
  });
}
