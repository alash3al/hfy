// The WHATWG URL subset under the link policy (lib/src/markdown/url.dart):
// serialization, host forms, IDNA labels, percent-encode sets and relative
// resolution. Every expectation was read from the browser URL implementation
// (node's `new URL`, the same standard ui.js calls), not derived by hand.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/markdown/url.dart';

String? _href(String raw, [String? base]) {
  final origin = base == null ? null : parseHfyUrl(base);
  return parseHfyUrl(raw, base: origin)?.href;
}

void main() {
  group('P-MD-011 host serialization (IPv6)', () {
    for (final (raw, want) in const [
      ('https://[::]/', 'https://[::]/'),
      ('https://[1:2:3:4:5:6:7:8]/', 'https://[1:2:3:4:5:6:7:8]/'),
      ('https://[1:0:0:2:0:0:0:3]/', 'https://[1:0:0:2::3]/'),
      ('https://[::ffff:1.2.3.4]/', 'https://[::ffff:102:304]/'),
      ('https://[::1]:80/', 'https://[::1]:80/'),
      ('https://[::1]x/', null),
      ('https://[1::2::3]/', null),
      ('https://[1.2.3.4]/', null),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-MD-011 host serialization (IPv4)', () {
    for (final (raw, want) in const [
      ('https://0x7f.1', 'https://127.0.0.1/'),
      ('https://1.2.3', 'https://1.2.0.3/'),
      ('https://1.0x2.3', 'https://1.2.0.3/'),
      ('https://0xffffffff', 'https://255.255.255.255/'),
      ('https://0x100000000', null),
      ('https://1.2.3.4.5', null),
      ('https://1.2.3.09', null),
      ('https://1..2', null),
      ('https://999999999999/', null),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-MD-011 IDNA: ASCII hosts map, a non-ASCII host fails closed', () {
    for (final (raw, want) in const [
      ('https://Ｅｘａｍｐｌｅ.com', 'https://example.com/'),
      ('https://a。b', 'https://a.b/'),
      ('https://%41.example/', 'https://a.example/'),
      ('https://ünicode.example', null),
      ('https://ß.example', null),
      ('https://ς.example', null),
      ('https://ﬁ.example', null),
      ('https://a\u00ad.example', null),
      ('https://\u200bx.example', null),
      ('https://%C3%BC.example', null),
      ('https://a\u202eb.example', null),
      ('https://\u3164.example', null),
      ('https://x\u0085.example', null),
      ('https://\u0000.example', null),
      ('https://a b/', null),
      ('https://a%20b/', null),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-MD-011 IDNA: xn-- labels of ASCII hosts are checked', () {
    for (final (raw, want) in const [
      ('https://xn--ls8h.example', 'https://xn--ls8h.example/'),
      ('https://xn--mnchen-3ya.example', 'https://xn--mnchen-3ya.example/'),
      ('https://x.xn--zca.example', 'https://x.xn--zca.example/'),
      ('https://xn--a.example', null),
      ('https://xn--Ab.example', null),
      ('https://xn--.example', null),
      ('https://xn--ab-g4t.example', null),
      ('https://xn--ab-m1t.example', null),
      ('https://xn--ab-j1t.example', null),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-MD-011 percent-encode sets and paths', () {
    for (final (raw, want) in const [
      ('https://x/a^b', 'https://x/a%5Eb'),
      ('https://x/a|b[c]', 'https://x/a|b[c]'),
      ('https://x/a\\b', 'https://x/a/b'),
      (
        'https://x/?q=a b\'"<>`#f"<>` ',
        'https://x/?q=a%20b%27%22%3C%3E`#f%22%3C%3E%60',
      ),
      ('https://x/ä/ö?ü#ß', 'https://x/%C3%A4/%C3%B6?%C3%BC#%C3%9F'),
      ('https://x/a/../b/./c/%2e%2E/d', 'https://x/b/d'),
      ('https://x/%zz', 'https://x/%zz'),
      ('https://x/a%2Fb', 'https://x/a%2Fb'),
      ('https://x/`{}|', 'https://x/%60%7B%7D|'),
      ('https://x/😀', 'https://x/%F0%9F%98%80'),
      ('https://x?', 'https://x/?'),
      ('https://x#', 'https://x/#'),
      ('https:///x', 'https://x/'),
      ('https:\\\\x\\y', 'https://x/y'),
      ('https://user:p@ss@h/', 'https://user:p%40ss@h/'),
      ('https://a:@c/', 'https://a@c/'),
      ('https://h:0080/', 'https://h:80/'),
      ('https://h:65536/', null),
      ('https://x:port/', null),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-MD-011 opaque mailto paths', () {
    for (final (raw, want) in const [
      ('mailto:a b@x', 'mailto:a b@x'),
      ('mailto:a b?c d#e f', 'mailto:a b?c%20d#e%20f'),
      ('mailto:a ?x', 'mailto:a%20?x'),
      ('mailto:a #x', 'mailto:a%20#x'),
      ('mailto:a  b', 'mailto:a  b'),
      ('mailto:a\tb', 'mailto:ab'),
      ('mailto:ü@x', 'mailto:%C3%BC@x'),
      ('mailto:', 'mailto:'),
      ('mailto:?x', 'mailto:?x'),
      ('mailto://x@y', 'mailto://x@y'),
    ]) {
      test(raw, () => expect(_href(raw), want));
    }
  });

  group('P-CRD-004 relative resolution against an http(s) base', () {
    const base = 'https://embed.example/a/b?c#d';
    for (final (raw, want) in const [
      ('?x', 'https://embed.example/a/b?x'),
      ('', 'https://embed.example/a/b?c'),
      ('#f', 'https://embed.example/a/b?c#f'),
      ('.', 'https://embed.example/a/'),
      ('..', 'https://embed.example/'),
      ('./', 'https://embed.example/a/'),
      ('//x/y', 'https://x/y'),
      ('\\\\x\\y', 'https://x/y'),
      ('x y:z', 'https://embed.example/a/x%20y:z'),
      ('https:x', 'https://embed.example/a/x'),
      ('http:x', 'http://x/'),
    ]) {
      test('"$raw" against $base', () => expect(_href(raw, base), want));
    }
    test('a relative reference against an opaque base fails', () {
      expect(_href('x', 'mailto:a@b'), isNull);
    });
    test('a fragment against an opaque base keeps the base path', () {
      expect(_href('#f', 'mailto:a@b'), 'mailto:a@b#f');
    });
    test('a scheme-less base cannot anchor a relative reference', () {
      expect(_href('x', 'embed/page'), isNull);
    });
  });

  group('the parser keeps the scheme it parsed', () {
    test('mixed-case scheme is lowercased', () {
      expect(parseHfyUrl('HTTPS://X.EXAMPLE/OK')?.scheme, 'https');
    });
    test('an unsupported scheme still parses, the policy rejects it', () {
      expect(parseHfyUrl('ftp://x.example/')?.scheme, 'ftp');
    });
  });
}
