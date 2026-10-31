// Adversarial input (PARITY P-MD-021 for the parse side; the spec's resource
// bounds): every pathological shape finishes in well under a second, never
// throws, and never overflows the stack. The ui.js regexes and the forward
// emphasis scan are quadratic or worse on these; the Dart port is linear
// (see the notes at the top of lib/src/markdown/parser.dart).

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/nodes.dart';
import 'package:hfy_flutter/src/markdown/parser.dart';

const int _n = 100000;

String _repeat(String s, int times) => List.filled(times, s).join();

/// Parses [input], asserting it returns within the budget, and returns the tree.
List<HfyMdBlock> _timed(String name, String input) {
  final watch = Stopwatch()..start();
  final tree = hfyMdParse(input);
  watch.stop();
  expect(
    watch.elapsedMilliseconds,
    lessThan(1000),
    reason:
        '$name took ${watch.elapsedMilliseconds} ms on ${input.length} chars',
  );
  return tree;
}

void _expectLiteralParagraph(List<HfyMdBlock> tree, String text) {
  expect(tree, hasLength(1));
  final p = tree.single as HfyMdParagraph;
  expect(p.toJson(), {
    't': 'p',
    'c': [
      {'t': 'text', 'v': text},
    ],
  });
}

void main() {
  test('P-MD unterminated ** over 100k chars stays literal and fast', () {
    final input = '**${_repeat('a', _n)}';
    _expectLiteralParagraph(_timed('unterminated **', input), input);
  });

  test('P-MD a run of 100k open brackets stays literal and fast', () {
    final input = _repeat('[', _n);
    _expectLiteralParagraph(_timed('[[[[', input), input);
  });

  test('P-MD unbalanced link openers stay literal and fast', () {
    _timed('[a[a', _repeat('[a', _n ~/ 2));
  });

  test(
    'P-MD 100k backticks open an empty fence, 100k underscores are one rule',
    () {
      final ticks = _timed('backticks', _repeat('`', _n));
      expect((ticks.single as HfyMdCodeBlock).toJson(), {
        't': 'pre',
        'lang': '',
        'text': '',
      });
      final unders = _timed('underscores', _repeat('_', _n));
      expect(unders.single, isA<HfyMdRule>());
    },
  );

  test('P-MD alternating emphasis openers over 100k chars', () {
    _timed('_a x50k', _repeat('_a', _n ~/ 2));
    _timed('*a x50k', _repeat('*a', _n ~/ 2));
    _timed('***a x25k', _repeat('***a', _n ~/ 4));
  });

  test('P-MD-026 the combinatorial forward-scan shape stays linear', () {
    // ui.js walks every candidate closer afresh: ~4 s at 40 groups and
    // unbounded beyond. The memoised closer answers each position once.
    _timed('spaced-star', '*x${_repeat(' **x', 20000)}');
    _timed('spaced-underscore', '*x${_repeat(' _x', 20000)}');
    _timed('mixed closers', _repeat('*_`', _n ~/ 3));
  });

  test('P-MD-012 a long trailing punctuation run in a bare URL', () {
    final input = 'https://x.example/${_repeat('.', _n)}b';
    _timed('dots-trim', input);
    _timed('closing parens', 'https://x.example/${_repeat(')', 50000)}');
  });

  test('P-MD-012 many bare-URL starts inside one token', () {
    _timed('https://x. x50k', _repeat('https://x.', _n ~/ 9));
    _timed('https://x.https://x', 'https://x${_repeat('.https://x', 10000)}');
    _timed('angle autolinks', _repeat('<https://a', _n ~/ 9));
  });

  test(
    'P-MD-003 a long fence-looking line with a backtick never backtracks',
    () {
      final input = '```${_repeat('a', _n)}`';
      _expectLiteralParagraph(_timed('fence-spaces', input), input);
    },
  );

  test('P-MD-004 a heading whose body is 100k spaces', () {
    final tree = _timed('heading-spaces', '# ${_repeat(' ', _n)}x');
    expect((tree.single as HfyMdHeading).toJson(), {
      't': 'h',
      'level': 1,
      'c': [
        {'t': 'text', 'v': 'x'},
      ],
    });
  });

  test('P-MD-005 long rule-like lines with a stray character', () {
    _timed('dash-spaces', '-${_repeat(' -', 50000)}x');
    _timed('dash-spaces-u2028', '-${_repeat(' ', _n)}\u2028');
    _timed('dash-run', '-${_repeat('-', _n)}x');
  });

  test('P-MD-007 a 2000-level nested list finishes and is depth-capped', () {
    final lines = [for (var k = 0; k < 2000; k++) '${_repeat(' ', 2 * k)}- x'];
    final tree = _timed('nested lists', lines.join('\n'));
    expect(_listDepth(tree), hfyMdDepthLimit + 1);
  });

  test('P-MD-007 10k nested list markers on one line are capped and fast', () {
    final tree = _timed('nested markers', '${_repeat('- ', 10000)}x');
    expect(_listDepth(tree), hfyMdDepthLimit + 1);
    final ordered = _timed('nested ordered', '${_repeat('1. ', 10000)}x');
    expect(_listDepth(ordered), hfyMdDepthLimit + 1);
  });

  test('P-MD-002 10k nested quotes are capped at the depth limit', () {
    final tree = _timed('nested quotes', '${_repeat('> ', 10000)}x');
    expect(_quoteDepth(tree), hfyMdDepthLimit);
  });

  test('P-MD-009 many inline code runs of distinct lengths', () {
    final input = [for (var k = 1; k <= 440; k++) '${_repeat('`', k)}a'].join();
    _timed('code runs', input);
  });

  test('P-MD-006 a paragraph of 20k lines', () {
    _timed('paragraph lines', _repeat('line\n', 20000));
  });

  test('P-MD-010 deep emphasis and link interleavings over 100k chars', () {
    _timed('star-bracket', _repeat('[*', 30000));
    _timed('link dest parens', '[t](https://x${_repeat('(', 50000)}');
    _timed('nested strong', _repeat('**a *b ', 10000));
  });

  test('P-MD-022 a 100k-char input is parsed without mutation or throw', () {
    final input = '${_repeat('*a _b` ', 12000)}\n# h\n\n> q';
    final first = _timed('mixed 100k', input);
    final again = hfyMdParse(input);
    expect(
      [for (final b in again) b.toJson()],
      [for (final b in first) b.toJson()],
    );
  });
}

int _listDepth(List<HfyMdBlock> tree) {
  var deepest = 0;
  void walkBlocks(List<HfyMdBlock> blocks, int depth) {
    for (final b in blocks) {
      if (b is HfyMdList) {
        if (depth + 1 > deepest) deepest = depth + 1;
        for (final item in b.items) {
          walkBlocks(item, depth + 1);
        }
      } else if (b is HfyMdQuote) {
        walkBlocks(b.children, depth);
      }
    }
  }

  walkBlocks(tree, 0);
  return deepest;
}

int _quoteDepth(List<HfyMdBlock> tree) {
  var depth = 0;
  var blocks = tree;
  while (blocks.length == 1 && blocks.single is HfyMdQuote) {
    depth++;
    blocks = (blocks.single as HfyMdQuote).children;
  }
  return depth;
}
