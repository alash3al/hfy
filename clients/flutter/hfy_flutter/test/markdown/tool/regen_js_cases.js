// Regenerates the JS-oracle expectations used by the markdown tests:
//   test/markdown/js_parity_cases.json  parser trees (hfyMdParse)
//   test/markdown/js_link_cases.json    hfySafeHref / hfySafeUrl results
//   test/markdown/documented_divergences.json  links the policy refuses on
//                                       purpose, with the browser's answer
// Every expectation is produced by the REAL internal/httpapi/embed/ui.js,
// loaded with the same DOM stubs as fixtures_test.go, never written by hand.
//
//   node test/markdown/tool/regen_js_cases.js      (from the package root)

const fs = require('fs');
const path = require('path');

function repoRoot(start) {
  let dir = start;
  for (;;) {
    if (fs.existsSync(path.join(dir, 'internal/httpapi/embed/ui.js'))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) throw new Error('internal/httpapi/embed/ui.js not found');
    dir = parent;
  }
}

const root = repoRoot(__dirname);
const src = fs.readFileSync(path.join(root, 'internal/httpapi/embed/ui.js'), 'utf8');

function load(href) {
  const win = { addEventListener() {} };
  win.parent = win;
  const docs = { addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; } };
  const loc = { href: href || 'https://embed.example/', origin: 'https://embed.example' };
  const f = new Function('window', 'document', 'navigator', 'requestAnimationFrame', 'location',
    src + '\n;return { parse: hfyMdParse, safeHref: hfySafeHref, safeUrl: hfySafeUrl };');
  return f(win, docs, { language: 'en' }, function () {}, loc);
}

const md = [
  // fences: closer length, unterminated, backtick-in-info, indent, tilde
  ['fence-longer-closer', '```\na\n````\nb'],
  ['fence-shorter-not-closer', '````\na\n```\nb\n````'],
  ['fence-tilde-run-closes-only-tilde', '~~~\na\n```\n~~~'],
  ['fence-backtick-after-info-no-fence', '```a`b\nx\n```'],
  ['fence-indent-dedent-tab', '  ```\n\t\tx\n  ```'],
  ['fence-info-with-tilde', '```~x\ny\n```'],
  ['fence-spaces-then-backtick-info', '``` a `b\n```'],
  // thematic breaks
  ['hr-underscores-spaced', '_ _ _'],
  ['hr-tab-separated', '*\t*\t*'],
  ['hr-four-spaces-not-rule', '    ***'],
  ['hr-three-spaces-rule', '   ***'],
  ['hr-mixed-markers-not-rule', '-*-'],
  ['hr-two-markers-not-rule', '--'],
  ['hr-trailing-text-not-rule', '--- x'],
  ['hr-long-spaced', '-' + ' '.repeat(50) + '-' + ' '.repeat(5) + '-'],
  ['hr-long-spaced-then-text', '-' + ' -'.repeat(40) + 'x'],
  // list items and line separators
  ['item-u2028-in-content', '- a\u2028b'],
  ['item-empty-u2028', '-\u2028'],
  ['item-ordered-u2028', '1. x\u2028y'],
  ['item-long-spaces-u2028', '-' + ' '.repeat(40) + '\u2028'],
  ['item-gap-three', '-   x'],
  ['item-gap-six-caps-to-one', '-      x\n       y'],
  ['item-tab-gap', '-\tx\n\ty'],
  ['item-start-three', '3. a\n4. b'],
  ['item-paren-marker', '1) a\n2) b'],
  ['item-type-switch', '* a\n- b'],
  ['item-loose-blank', '- a\n\n  b'],
  ['item-two-blank-lines', '- x\n\n\n- y'],
  ['item-lazy-after-blank', '- a\n\nlazy'],
  ['item-ordered-not-interrupt', 'para\n2. no\n3. no'],
  ['item-interrupt-with-one', 'para\n1. yes'],
  // headings
  ['heading-tab-after-hash', '#\tTitle'],
  ['heading-closing-run-needs-space', '# a#'],
  ['heading-empty-hashes', '#   #'],
  ['heading-three-hash-closer', '### ###'],
  ['heading-u2028-blocks', '#\u2028x'],
  ['heading-seven-hashes', '####### x'],
  // quotes
  ['quote-lazy-no-space', '>no space\n>  two'],
  ['quote-three-spaces', '>   x'],
  ['quote-paragraphs-blank', '> a\n>\n> b\n\nc'],
  // links and autolinks
  ['link-angle-title', '[a](<b c> "t")'],
  ['link-plain-title-single', "[a](b 'x y')"],
  ['link-extra-after-title', '[a](https://x.example "t" extra)'],
  ['link-spaces-around-dest', '[a]( https://x.example )'],
  ['link-escaped-paren-dest', '[a](https://x.example\\))'],
  ['link-query-parens', '[a](https://x.example?q=(1))'],
  ['link-mailto', '[a](mailto:x@y.z)'],
  ['link-mailto-upper-scheme', '[a](MAILTO:x@y.z)'],
  ['link-javascript-dest', '[a](javascript:alert(1))'],
  ['link-reference-style', '[a][b] [c]'],
  ['link-code-label', '[`x`](https://x.example)'],
  ['link-dest-u2028', '[a](https://x\u2028.example)'],
  ['autolink-upper-scheme', '<HTTP://A.B>'],
  ['autolink-mailto-plain', '<mailto:a@b>'],
  ['autolink-space-inside', '<https://a b>'],
  ['autolink-extra-gt', '<https://a>b>'],
  ['bare-trailing-underscore', 'https://x.example/a_b_'],
  ['bare-paren-wrapped', '(https://x.example)'],
  ['bare-unbalanced-paren', 'https://x.example/a)b)'],
  ['bare-balanced-trailing', 'https://x.example/a(b)c)'],
  ['bare-trailing-dot', 'https://x.example.'],
  ['bare-query-fragment', 'x https://a.b/c?d=e&f=g#h'],
  ['bare-punct-run', 'https://x.example' + '.'.repeat(20) + 'b'],
  // emphasis: forward-scan parity (P-MD-026)
  ['em-nested-strong', '**a *b* c**'],
  ['em-nested-other-way', '*a **b** c*'],
  ['em-triple-run', '***a***'],
  ['em-unbalanced-tail', '*a**'],
  ['em-unbalanced-head', '**a*'],
  ['em-underscore-word', '_a_b_'],
  ['em-asterisk-word', 'a*b*'],
  ['em-multi-line', '*a\nb*'],
  ['em-mixed-closers', '__a *b__ c*'],
  ['em-code-inside', '*a `b*` c*'],
  ['em-run-split-three', '**a***'],
  ['em-run-three-open-two-close', '***a**'],
  ['em-deep-chain', '*a *b *c *d *e *f *g h* f* e* d* c* b* a*'],
  ['em-spaced-openers', '*x' + ' **x'.repeat(8)],
  ['em-spaced-underscores', '*x' + ' _x'.repeat(8)],
  // inline limits: a link label scan stops at 1000 chars, a destination at 2048
  ['link-label-at-limit', '[' + 'a'.repeat(998) + '](https://x.example)'],
  ['link-label-past-limit', '[' + 'a'.repeat(999) + '](https://x.example)'],
  ['link-dest-at-limit', '[t](https://x.example/' + 'a'.repeat(2047 - 'https://x.example/'.length) + ')'],
  ['link-dest-past-limit', '[t](https://x.example/' + 'a'.repeat(2048 - 'https://x.example/'.length) + ')'],
  // flanking: a delimiter followed by white space never opens, and one
  // preceded by white space never closes (ui.js:568, 585-589)
  ['em-opener-followed-by-space', 'a * b * c'],
  ['em-opener-run-two-space', 'x ** y ** z'],
  ['em-opener-space-then-closer', 'a * b*'],
  ['em-opener-run-two-space-then-closer', 'x ** y** z'],
  ['em-opener-tab', '*\tx*'],
  ['em-opener-newline', '*\nx*'],
  ['em-closer-preceded-by-space', 'a *b * c'],
  ['em-nested-opener-space', '*a *b * c* d'],
  ['em-triple-opener-space', '*** a ***'],
  ['em-underscore-opener-space', 'x _ y _ z'],
  ['em-underscore-closer-word', 'a _b _c'],
  ['em-closer-run-three-space', '*a *** b'],
  // code spans and escapes
  ['code-double-run-inside', '`a``b`'],
  ['code-single-inside-double', '``a`b``'],
  ['code-only-spaces', '` `'],
  ['code-padded', '`  x  `'],
  ['code-fence-like-run', '``` a ```'],
  ['escape-double-backslash', '\\\\'],
  ['escape-letter', '\\a'],
  ['escape-backtick', '\\`code\\`'],
  // small pathological shapes (kept small: the JS oracle is slow on them)
  ['bracket-run', '[[[[[[[[[[a'],
  ['star-run-unclosed', '*' + '*a'.repeat(12)],
  ['quote-deep-lines', '> > > > > > > x\n> > > > > > > y'],
  ['list-deep-five', '- a\n  - b\n    - c\n      - d\n        - e'],
];

const links = [
  ['href', 'https://x.example'],
  ['href', 'http://x.example/a b'],
  ['href', 'HTTPS://X.EXAMPLE/OK'],
  ['href', 'https://user:pa ss@x.example/'],
  ['href', 'https://x.example:443/'],
  ['href', 'https://x.example:8443/p'],
  ['href', 'https://x.example:0080/'],
  ['href', 'https://x.example:65536/'],
  ['href', 'https://x.example/%zz?q=a b#f g'],
  ['href', 'https://x.example/a/../b/./c/%2e%2E/d'],
  ['href', 'https://[::1]:8080/x'],
  ['href', 'https://[1:0:0:2:0:0:0:3]/'],
  ['href', 'https://[::ffff:1.2.3.4]/'],
  ['href', 'https://[1::2::3]/'],
  ['href', 'https://[1.2.3.4]/'],
  ['href', 'https://0x7f.1/'],
  ['href', 'https://1.2.3.4.5/'],
  ['href', 'https://4294967295/'],
  ['href', 'https://4294967296/'],
  ['href', 'https://ab.cd.ef.gh.ij/'],
  ['href', 'https://a..b/'],
  ['href', 'https://./'],
  ['href', 'https://x.example./'],
  ['href', 'https://Ｅｘａｍｐｌｅ.com'],
  ['href', 'https://a。b/'],
  ['href', 'https://x\u0085.example/'],
  ['href', 'https://xn--ls8h.example/'],
  ['href', 'https://xn--a.example/'],
  ['href', 'https://xn--Ab.example/'],
  ['href', 'https://x.xn--zca.example/'],
  ['href', 'https://user:p@ss@h/'],
  ['href', 'https://a:@c/'],
  ['href', 'https://a%41@c/'],
  ['href', 'https://x/?\'q\''],
  ['href', 'https://x/a^b|c[d]e\\f'],
  ['href', 'https://x/a/`{}'],
  ['href', 'https://x/\u007f'],
  ['href', 'https://x/a\u{10ffff}'],
  ['href', 'mailto:a@x.example'],
  ['href', 'mailto:a ?x'],
  ['href', 'mailto:a #x'],
  ['href', 'mailto:'],
  ['href', 'mailto://x@y'],
  ['href', 'mailto:ü@x.example'],
  ['href', 'mailto:/x'],
  ['href', 'MAILTO:a@b'],
  ['href', 'javascript:alert(1)'],
  ['href', 'JaVaScRiPt:alert(1)'],
  ['href', 'data:text/html,x'],
  ['href', 'vbscript:msgbox(1)'],
  ['href', 'file:///etc/passwd'],
  ['href', 'ftp://x.example/'],
  ['href', '//evil.example/x'],
  ['href', '/relative/path'],
  ['href', 'relative'],
  ['href', ' https://x.example'],
  ['href', '\thttps://x.example'],
  ['href', 'https://x.example\t\n/a'],
  ['href', 'https\t://x.example'],
  ['href', 'https:x'],
  ['href', 'https://'],
  ['href', 'http://'],
  ['href', 'mailto'],
  ['href', ''],
  ['href', 'hTTps://x.example'],
  ['href', 'https://x.example/\u2028'],
  ['href', 'https://x.example/\u0001'],
  ['href', 'https://x.example\u0000'],
  ['href', 'https://a\u0000b/'],
  ['href', 'https://a%20b/'],
  ['href', 'https://a b/'],
  ['href', 'https://a<b/'],
  ['url', '/v1/files/a b', 'https://embed.example/'],
  ['url', '/v1/files/x?sig=1#frag', 'https://embed.example/'],
  ['url', '//cdn.example/i.png', 'https://embed.example/'],
  ['url', '//cdn.example/i.png', 'http://embed.example/'],
  ['url', 'javascript:alert(1)', 'https://embed.example/'],
  ['url', 'JAVASCRIPT:alert(1)', 'https://embed.example/'],
  ['url', 'data:image/png;base64,AAAA', 'https://embed.example/'],
  ['url', 'vbscript:x', 'https://embed.example/'],
  ['url', 'blob:https://embed.example/uuid', 'https://embed.example/'],
  ['url', 'https://cdn.example/a.png', 'https://embed.example/'],
  ['url', 'http://cdn.example/a.png', 'https://embed.example/'],
  ['url', 'ftp://cdn.example/a.png', 'https://embed.example/'],
  ['url', '?q=1', 'https://embed.example/a/b?c#d'],
  ['url', '#frag', 'https://embed.example/a/b?c#d'],
  ['url', '', 'https://embed.example/'],
  ['url', '../../x', 'https://embed.example/a/b/c'],
  ['url', 'x y:z', 'https://embed.example/'],
  ['url', 'https:x', 'https://embed.example/a/b'],
  ['url', 'http:x', 'https://embed.example/a/b'],
  ['url', '\\\\h\\p', 'https://embed.example/'],
  ['url', 'x', 'https://u:p@embed.example/b'],
  ['url', 'x', 'https://embed.example:8443/b'],
  ['url', 'x', 'http://embed.example/b'],
  ['url', 'x', 'mailto:a@b'],
  ['url', '#f', 'mailto:a@b'],
  ['url', 'x', 'ftp://h/a'],
  ['url', '/v1/files/%E2%82%AC.png', 'https://embed.example/'],
  ['url', 'https://x.example/\u0001', 'https://embed.example/'],
  ['url', ' /v1/files/a ', 'https://embed.example/'],
];

// Links the policy refuses on purpose (url.dart: a non-ASCII host fails closed;
// links.dart: a mailto path keeps no space). `browser` is what the real ui.js
// returns; the Dart test asserts the fail-closed result and records the gap.
const divergences = [
  // the eleven attack hosts: the browser refuses some and links others elsewhere
  ['attack', 'href', 'https://a\u202Eb.example/'],
  ['attack', 'href', 'https://a\u200Db.example/'],
  ['attack', 'href', 'https://a\u200Cb.example/'],
  ['attack', 'href', 'https://\u06611.example/'],
  ['attack', 'href', 'https://a\u{E0001}b.example/'],
  ['attack', 'href', 'https://\u2460.com/'],
  ['attack', 'href', 'https://\u{1D41A}.example/'],
  ['attack', 'href', 'https://\u00B9.example/'],
  ['attack', 'href', 'https://g\u0307.example/'],
  ['attack', 'href', 'https://\u3164.example/'],
  ['attack', 'href', 'https://\u115F.example/'],
  // non-ASCII hosts the browser maps to xn-- labels
  ['non-ascii-host', 'href', 'https://ünicode.example/'],
  ['non-ascii-host', 'href', 'https://😊.example/'],
  ['non-ascii-host', 'href', 'https://ß.example/'],
  ['non-ascii-host', 'href', 'https://ẞ.example/'],
  ['non-ascii-host', 'href', 'https://ǅ.example/'],
  ['non-ascii-host', 'href', 'https://ǆ.example/'],
  ['non-ascii-host', 'href', 'https://ﬁ.example/'],
  ['non-ascii-host', 'href', 'https://ſ.example/'],
  ['non-ascii-host', 'href', 'https://İ.example/'],
  ['non-ascii-host', 'href', 'https://a\u00ad.example/'],
  ['non-ascii-host', 'href', 'https://\u200bx.example/'],
  ['non-ascii-host', 'href', 'https://%C3%BC.example/'],
  ['non-ascii-host', 'url', 'https://ünicode.example/i.png', 'https://embed.example/'],
  // a mailto path with a raw space: the browser keeps the space
  ['raw-space', 'href', 'mailto:a b@x.example'],
];
const divergenceCases = divergences.map(([group, kind, input, base]) =>
  kind === 'href'
    ? { group, kind, input, browser: load().safeHref(input) }
    : { group, kind, input, base, browser: load(base).safeUrl(input) });

const parityCases = md.map(([name, input]) => ({ name, input, tree: load().parse(input) }));
const linkCases = links.map((row) => {
  if (row[0] === 'href') return { kind: 'href', input: row[1], expected: load().safeHref(row[1]) };
  return { kind: 'url', input: row[1], base: row[2], expected: load(row[2]).safeUrl(row[1]) };
});

const dir = path.join(root, 'clients/flutter/hfy_flutter/test/markdown');
fs.writeFileSync(path.join(dir, 'js_parity_cases.json'), JSON.stringify(parityCases, null, 2) + '\n');
fs.writeFileSync(path.join(dir, 'js_link_cases.json'), JSON.stringify(linkCases, null, 2) + '\n');
fs.writeFileSync(path.join(dir, 'documented_divergences.json'), JSON.stringify(divergenceCases, null, 2) + '\n');
console.log(`wrote ${parityCases.length} parser cases, ${linkCases.length} link cases and ${divergenceCases.length} divergences`);
