// Link safety (PARITY P-MD-011, P-CRD-004 for the card/attachment rule): the
// results of hfySafeHref and hfySafeUrl must equal the real ui.js functions
// (ui.js:302-311) on every row of js_link_cases.json, and the named rows below
// pin the scheme classes the policy exists for.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/links.dart';

import 'support/data.dart';

final Uri _page = Uri.parse('https://embed.example/');

void main() {
  final rows = markdownDataCases('js_link_cases.json');

  test('P-MD-011: the oracle file holds its 114 link rows', () {
    expect(rows, hasLength(114));
  });

  for (final row in rows) {
    final kind = row['kind'] as String;
    final input = row['input'] as String;
    if (kind == 'href') {
      test('P-MD-011 hfySafeHref ${_show(input)}', () {
        expect(hfySafeHref(input), row['expected']);
      });
    } else {
      final base = Uri.parse(row['base'] as String);
      test('P-CRD-004 hfySafeUrl ${_show(input)} against ${row['base']}', () {
        expect(hfySafeUrl(input, base), row['expected']);
      });
    }
  }

  group('P-MD-011 unsafe schemes never become links', () {
    for (final raw in const [
      'javascript:alert(1)',
      'JaVaScRiPt:alert(1)',
      'data:text/html,<script>alert(1)</script>',
      'vbscript:msgbox(1)',
      'file:///etc/passwd',
      'ftp://x.example/',
      '//evil.example/x',
      '/relative/path',
      'relative',
      '',
    ]) {
      test(_show(raw), () {
        expect(hfySafeHref(raw), isNull);
      });
    }
  });

  group('P-MD-011 whitespace and control characters', () {
    test('a leading space or tab disqualifies the link', () {
      expect(hfySafeHref(' https://x.example'), isNull);
      expect(hfySafeHref('\thttps://x.example'), isNull);
    });
    test('a tab inside the scheme disqualifies the link', () {
      expect(hfySafeHref('https\t://x.example'), isNull);
    });
    test(
      'tabs and newlines inside the path are stripped, as the URL parser does',
      () {
        expect(hfySafeHref('https://x.example\t\n/a'), 'https://x.example/a');
      },
    );
    test('a trailing C0 control is stripped by the URL parser', () {
      expect(hfySafeHref('https://x.example/\u0001'), 'https://x.example/');
    });
  });

  group('P-MD-011 safe absolute URLs come back serialized', () {
    test('http(s) and mailto pass with the WHATWG href', () {
      expect(hfySafeHref('https://x.example'), 'https://x.example/');
      expect(hfySafeHref('HTTP://X.EXAMPLE/OK'), 'http://x.example/OK');
      expect(hfySafeHref('mailto:a@x.example'), 'mailto:a@x.example');
    });
  });

  group('P-CRD-004 card and attachment URLs resolve against the page', () {
    test('a server-relative download resolves to the page origin', () {
      expect(
        hfySafeUrl('/v1/files/a b', _page),
        'https://embed.example/v1/files/a%20b',
      );
    });
    test('a protocol-relative url keeps the base scheme', () {
      expect(
        hfySafeUrl('//cdn.example/i.png', _page),
        'https://cdn.example/i.png',
      );
      expect(
        hfySafeUrl('//cdn.example/i.png', Uri.parse('http://embed.example/')),
        'http://cdn.example/i.png',
      );
    });
    test('unsafe or unparsable urls are the empty string, never null', () {
      expect(hfySafeUrl('javascript:alert(1)', _page), '');
      expect(hfySafeUrl('data:image/png;base64,AAAA', _page), '');
      expect(hfySafeUrl('ftp://cdn.example/a.png', _page), '');
      expect(hfySafeUrl(null, _page), '');
      expect(hfySafeUrl('', _page), '');
    });
    test('a relative base cannot resolve anything', () {
      expect(hfySafeUrl('/v1/files/x', Uri.parse('embed/page')), '');
    });
  });
}

String _show(String s) =>
    s.isEmpty ? '(empty)' : s.replaceAll('\t', r'\t').replaceAll('\n', r'\n');
