// The markdown parser (PARITY P-MD-001..003, P-MD-006..012, P-MD-026): a port
// of `hfyMdParse` (ui.js:417-629) with the same regexes, the same block and
// inline rules and the same forward-scan emphasis. Output is the immutable
// tree in nodes.dart; the rendering half (hfyMdBuild) is not ported here.
//
// Departures from ui.js. The first four change no output (the golden, parity
// and fuzz runs agree with ui.js on every line); the last two are bounds,
// documented where they apply and tested in limits_test.dart:
//   - the emphasis closer scan is memoised per delimiter (ui.js walks it
//     afresh for every candidate, which is exponential on `*x` + ` **x`*N);
//   - the fence and list-item regexes are written so a failing line does not
//     backtrack quadratically (see [_fence] and [_item]);
//   - the thematic-break regex is a single pass ([_isRule]): ui.js's backref
//     loop overflows the regex engine on a long rule;
//   - trailing-punctuation trims in link destinations and bare URLs are plain
//     loops (the ui.js regexes are quadratic on runs of punctuation);
//   - a bare URL longer than 2048 characters after its scheme stays plain text
//     (ui.js links it whole): each candidate's scan is bounded ([_bare]);
//   - an input longer than [hfyMdMaxInput] is not parsed, see [hfyMdParse].
// Every character test works on UTF-16 code units, as the JS does.

import 'dart:math' show min;
import 'dart:typed_data' show Int32List;

import 'links.dart';
import 'nodes.dart';

/// Nesting cap for quotes, list items and inline recursion (ui.js:418,
/// HFY_MD_DEPTH). Past it, a quote or list item flattens to paragraph text.
const int hfyMdDepthLimit = 6;

final RegExp _crlf = RegExp(r'\r\n?');
// shortcut: ui.js's fence regex backtracks quadratically when the line holds a
// backtick after the run; the lookahead states "no backtick follows the run",
// which is the same set of lines, decided in one pass.
final RegExp _fence = RegExp(
  r'^ {0,3}(`{3,}|~{3,})(?![^`]*`)[ \t]*([^\s`]*)[^`]*$',
);
final RegExp _head = RegExp(r'^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$');
final RegExp _quote = RegExp(r'^ {0,3}> ?');
// shortcut: the `(?![ \t])` makes the white space run maximal, so a failing
// line (a U+2028 after it, say) cannot backtrack through every shorter run.
final RegExp _item = RegExp(
  r'^( {0,3})([-*+]|\d{1,9}[.)])(?:([ \t]+(?![ \t]))(.*))?$',
);
final RegExp _punct = RegExp(r'[!-\/:-@\[-`{-~]');
final RegExp _word = RegExp(r'[\p{L}\p{N}]', unicode: true);
final RegExp _auto = RegExp(
  r'<((?:https?:\/\/|mailto:)[^\s<>]+)>',
  caseSensitive: false,
);
// shortcut: at most 2048 characters after the scheme, the same cap as a link
// destination; the lookahead makes a longer run fail instead of truncating.
// Upgrade with a linear URL scan if long bare links must link.
final RegExp _bare = RegExp(
  r'''https?:\/\/[^\s<>"'`]{1,2048}(?![^\s<>"'`])''',
  caseSensitive: false,
);
final RegExp _destAngle = RegExp(
  r'''^<([^<>\n]*)>(?:\s+(?:"[^"]*"|'[^']*'))?$''',
);
final RegExp _destPlain = RegExp(
  r'''^(\S*)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?$''',
);

const int _lf = 0x0A;
const int _backtick = 0x60;
const int _backslash = 0x5C;
const int _star = 0x2A;
const int _underscore = 0x5F;
const int _lt = 0x3C;
const int _lbracket = 0x5B;
const int _rbracket = 0x5D;
const int _lparen = 0x28;
const int _rparen = 0x29;
const int _h = 0x68;
const int _upperH = 0x48;

/// Largest input (UTF-16 code units) that [hfyMdParse] parses in full. Longer
/// text is not parsed: see [hfyMdParse]. A documented deviation from ui.js,
/// which parses any length. It bounds the parser's memory: the emphasis memo
/// holds about 84 bytes per `*` or `_` in the scanned text (see [_Inline]).
const int hfyMdMaxInput = 262144;

/// Input size from which the widget phase should run [hfyMdParse] off the main
/// isolate (`compute` or `Isolate.run`). The parser stays synchronous and pure;
/// the caller decides where it runs.
const int hfyMdIsolateThreshold = 32768;

/// Parses markdown the way `hfyMdParse` does (ui.js:459): CRLF and CR become
/// LF, then blocks. Pure: no I/O, no rendering. Never throws on any string.
///
/// Text longer than [hfyMdMaxInput] comes back as plain paragraphs, one per run
/// of non-blank lines, with no inline or block syntax (see [_plainParagraphs]).
/// The cap bounds memory, not time, and not the block count: 5 MB of `a` and
/// blank lines gives about 1.7M blocks. The widget must render a capped number
/// of them (truncate, or build them lazily), never all at once.
///
/// Widget-phase obligations (see lib/src/markdown/README.md):
///   - run this off the main isolate (`compute` or `Isolate.run`) for text over
///     [hfyMdIsolateThreshold] characters (32 KiB);
///   - sanitize lone surrogates at the widget boundary, before the text is
///     parsed or shown, with `utf8.decode(utf8.encode(s))`: each one becomes
///     U+FFFD and valid pairs are kept (limits_test.dart pins this).
List<HfyMdBlock> hfyMdParse(String text) {
  if (text.length > hfyMdMaxInput) return _plainParagraphs(text);
  final lines = text.replaceAll(_crlf, '\n').split('\n');
  return _blocks(lines, 0);
}

// Past the cap: each run of non-blank lines (trimmed as paragraph lines are)
// becomes one paragraph of plain text, line breaks kept.
List<HfyMdBlock> _plainParagraphs(String text) {
  final out = <HfyMdBlock>[];
  final para = <String>[];
  void flush() {
    if (para.isEmpty) return;
    out.add(HfyMdParagraph([HfyMdText(para.join('\n'))]));
    para.clear();
  }

  for (final line in text.replaceAll(_crlf, '\n').split('\n')) {
    final trimmed = _jsTrim(line);
    if (trimmed.isEmpty) {
      flush();
    } else {
      para.add(trimmed);
    }
  }
  flush();
  return out;
}

// ---- blocks (ui.js:429-519) ----

List<HfyMdBlock> _blocks(List<String> lines, int depth) {
  final out = <HfyMdBlock>[];
  final para = <String>[];
  var i = 0;

  void flush() {
    if (para.isEmpty) return;
    out.add(HfyMdParagraph(_Inline(para.join('\n')).parse(0, false)));
    para.clear();
  }

  while (i < lines.length) {
    final line = lines[i];
    if (_jsTrim(line).isEmpty) {
      flush();
      i++;
      continue;
    }
    if (para.isNotEmpty && !_startsBlock(line, true)) {
      para.add(_jsTrim(line));
      i++;
      continue;
    }
    flush();
    final fence = _fence.firstMatch(line);
    if (fence != null) {
      final run = fence.group(1)!;
      final close = RegExp('^ {0,3}${run[0]}{${run.length},}[ \\t]*\$');
      final ind = _indent(line);
      final body = <String>[];
      for (i++; i < lines.length && !close.hasMatch(lines[i]); i++) {
        body.add(_dedent(lines[i], ind));
      }
      i++; // an unterminated fence (still streaming) runs to the end
      out.add(HfyMdCodeBlock(lang: fence.group(2)!, text: body.join('\n')));
      continue;
    }
    final head = _head.firstMatch(line);
    if (head != null) {
      out.add(
        HfyMdHeading(
          min(head.group(1)!.length, 3),
          _Inline(head.group(2)!).parse(0, false),
        ),
      );
      i++;
      continue;
    }
    if (_isRule(line)) {
      out.add(const HfyMdRule());
      i++;
      continue;
    }
    if (_quote.hasMatch(line)) {
      final q = <String>[];
      while (i < lines.length && _quote.hasMatch(lines[i])) {
        q.add(lines[i].replaceFirst(_quote, ''));
        i++;
      }
      out.add(
        depth < hfyMdDepthLimit
            ? HfyMdQuote(_blocks(q, depth + 1))
            : HfyMdParagraph(_Inline(q.join('\n')).parse(0, false)),
      );
      continue;
    }
    if (_item.hasMatch(line)) {
      i = _list(lines, i, depth, out);
      continue;
    }
    para.add(_jsTrim(line));
    i++;
  }
  flush();
  return out;
}

// Whether `line` opens a block, so it cannot continue a paragraph or a lazy
// list item. Inside a paragraph a list only interrupts with a non-empty bullet
// or an ordered list starting at 1 (ui.js:451-458).
bool _startsBlock(String line, bool inParagraph) {
  if (_fence.hasMatch(line) ||
      _head.hasMatch(line) ||
      _isRule(line) ||
      _quote.hasMatch(line)) {
    return true;
  }
  final m = _item.firstMatch(line);
  if (m == null) return false;
  if (!inParagraph) return true;
  final content = m.group(4);
  final marker = m.group(2)!;
  return content != null &&
      content.isNotEmpty &&
      (marker == '-' ||
          marker == '*' ||
          marker == '+' ||
          marker == '1.' ||
          marker == '1)');
}

int _list(List<String> lines, int start, int depth, List<HfyMdBlock> out) {
  final first = _item.firstMatch(lines[start])!;
  final ordered = _containsDigit(first.group(2)!);
  final items = <List<String>>[];
  List<String>? cur;
  var i = start;
  var indent = 0;
  var nest = 0;
  var blank = false;
  while (i < lines.length) {
    final line = lines[i];
    final ind = _indent(line);
    if (cur != null && _jsTrim(line).isNotEmpty && ind >= nest) {
      cur.add(_dedent(line, min(ind, indent)));
      blank = false;
      i++;
      continue;
    }
    final m = _item.firstMatch(line);
    if (m != null && _containsDigit(m.group(2)!) == ordered && !_isRule(line)) {
      if (cur != null) items.add(cur);
      final gapText = m.group(3);
      final gap = gapText == null || gapText.length > 4 ? 1 : gapText.length;
      cur = [m.group(4) ?? ''];
      indent = m.group(1)!.length + m.group(2)!.length + gap;
      nest = m.group(1)!.length + min(m.group(2)!.length + 1, 2);
      blank = false;
      i++;
      continue;
    }
    if (_jsTrim(line).isEmpty) {
      var j = i + 1;
      while (j < lines.length && _jsTrim(lines[j]).isEmpty) {
        j++;
      }
      if (j < lines.length) {
        final next = _item.firstMatch(lines[j]);
        if (_indent(lines[j]) >= nest ||
            (next != null && _containsDigit(next.group(2)!) == ordered)) {
          cur!.add('');
          blank = true;
          i++;
          continue;
        }
      }
      break;
    }
    if (!blank && !_startsBlock(line, true)) {
      cur!.add(_jsTrim(line));
      i++;
      continue;
    }
    break;
  }
  if (cur != null) items.add(cur);
  final marker = first.group(2)!;
  out.add(
    HfyMdList(
      ordered: ordered,
      start: ordered ? int.parse(marker.substring(0, marker.length - 1)) : 1,
      items: [
        for (final item in items)
          depth < hfyMdDepthLimit
              ? _blocks(item, depth + 1)
              : [HfyMdParagraph(_Inline(item.join('\n')).parse(0, false))],
      ],
    ),
  );
  return i;
}

// `^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$` (ui.js:421) as a single pass. The
// ui.js regex keeps one backtrack frame per repeated marker, which overflows
// the regex engine on a long rule. Same lines: up to three spaces, one marker,
// then only that marker, spaces and tabs, with at least three markers in all.
bool _isRule(String line) {
  var i = 0;
  while (i < 3 && i < line.length && line.codeUnitAt(i) == 0x20) {
    i++;
  }
  if (i >= line.length) return false;
  final marker = line.codeUnitAt(i);
  if (marker != 0x2D && marker != 0x2A && marker != 0x5F) return false;
  var markers = 0;
  for (var j = i; j < line.length; j++) {
    final c = line.codeUnitAt(j);
    if (c == marker) {
      markers++;
    } else if (c != 0x20 && c != 0x09) {
      return false;
    }
  }
  return markers >= 3;
}

bool _containsDigit(String s) {
  for (var i = 0; i < s.length; i++) {
    final c = s.codeUnitAt(i);
    if (c >= 0x30 && c <= 0x39) return true;
  }
  return false;
}

int _indent(String line) {
  var n = 0;
  for (var i = 0; i < line.length; i++) {
    final c = line.codeUnitAt(i);
    if (c == 0x20) {
      n++;
    } else if (c == 0x09) {
      n += 4 - (n % 4);
    } else {
      break;
    }
  }
  return n;
}

String _dedent(String line, int cols) {
  var n = 0;
  var i = 0;
  while (i < line.length && n < cols) {
    final c = line.codeUnitAt(i);
    if (c == 0x20) {
      n++;
    } else if (c == 0x09) {
      n += 4 - (n % 4);
    } else {
      break;
    }
    i++;
  }
  return line.substring(i);
}

// ---- text helpers ----

// JS `String.prototype.trim` strips the ECMAScript white space set, which
// (unlike Dart's `trim`) does not include U+0085.
bool _isSpaceUnit(int c) =>
    (c >= 0x09 && c <= 0x0D) ||
    c == 0x20 ||
    c == 0xA0 ||
    c == 0x1680 ||
    (c >= 0x2000 && c <= 0x200A) ||
    c == 0x2028 ||
    c == 0x2029 ||
    c == 0x202F ||
    c == 0x205F ||
    c == 0x3000 ||
    c == 0xFEFF;

String _jsTrim(String s) {
  var start = 0;
  var end = s.length;
  while (start < end && _isSpaceUnit(s.codeUnitAt(start))) {
    start++;
  }
  while (end > start && _isSpaceUnit(s.codeUnitAt(end - 1))) {
    end--;
  }
  return start == 0 && end == s.length ? s : s.substring(start, end);
}

// `/[\p{L}\p{N}]/u` on one UTF-16 unit: a lone surrogate matches nothing.
bool _isWordUnit(int c) {
  if (c < 0x80) {
    return (c >= 0x30 && c <= 0x39) ||
        (c >= 0x41 && c <= 0x5A) ||
        (c >= 0x61 && c <= 0x7A);
  }
  return _word.hasMatch(String.fromCharCode(c));
}

// `/[\w\/]/`: the characters that make a following `http` part of a word.
bool _isBareBoundary(int c) =>
    c == 0x2F ||
    c == 0x5F ||
    (c >= 0x30 && c <= 0x39) ||
    (c >= 0x41 && c <= 0x5A) ||
    (c >= 0x61 && c <= 0x7A);

bool _isTrailingPunct(int c) =>
    c == 0x2E ||
    c == 0x2C ||
    c == 0x3B ||
    c == 0x3A ||
    c == 0x21 ||
    c == 0x3F ||
    c == 0x27 ||
    c == 0x22 ||
    c == 0x2A ||
    c == 0x5F ||
    c == 0x7E;

// `hfyTrimUrl` (ui.js:520-524): trailing punctuation goes, and so does a
// closing paren with no opener; each linear pass, never a regex.
String _trimUrl(String url) {
  var end = _trimPunctEnd(url, url.length);
  var open = 0;
  var close = 0;
  for (var i = 0; i < end; i++) {
    final c = url.codeUnitAt(i);
    if (c == _lparen) {
      open++;
    } else if (c == _rparen) {
      close++;
    }
  }
  while (end > 0 && url.codeUnitAt(end - 1) == _rparen && open < close) {
    end = _trimPunctEnd(url, end - 1);
    close--;
  }
  return url.substring(0, end);
}

int _trimPunctEnd(String s, int end) {
  var e = end;
  while (e > 0 && _isTrailingPunct(s.codeUnitAt(e - 1))) {
    e--;
  }
  return e;
}

// ---- inline (ui.js:525-619) ----

// Positions index one string; the closer memo lives with it, for the scan only.
final class _Inline {
  _Inline(this.s) : n = s.length;

  final String s;
  final int n;

  // Closer memo (see [_closer]): the delimiter at position j owns row
  // `_rowOf[j]` of `_memo`, 21 slots. Allocated by the first closer walk and
  // dropped when the scan ends, so only one string's memo is alive at a time.
  Int32List? _rowOf;
  Int32List? _memo;
  // The memo slots the walks in progress have passed, as one stack: a nested
  // walk pushes above its caller's slots and pops back to its own base.
  Int32List? _passed;
  int _passedCount = 0;

  List<HfyMdInline> parse(int depth, bool noLink) {
    if (depth > hfyMdDepthLimit) return [HfyMdText(s)];
    final out = <Object>[];
    final buf = StringBuffer();
    void emit(Object node) {
      if (buf.isNotEmpty) {
        out.add(HfyMdText(buf.toString()));
        buf.clear();
      }
      out.add(node);
    }

    var i = 0;
    while (i < n) {
      final c = s.codeUnitAt(i);
      if (c == _backslash && i + 1 < n && _punct.hasMatch(s[i + 1])) {
        buf.writeCharCode(s.codeUnitAt(i + 1));
        i += 2;
        continue;
      }
      if (c == _lf) {
        emit(const HfyMdLineBreak());
        i++;
        continue;
      }
      if (c == _backtick) {
        final r = _run(i, _backtick);
        final end = _findRun(i + r, _backtick, r);
        if (end == -1) {
          buf.write(s.substring(i, i + r));
          i += r;
          continue;
        }
        var code = s.substring(i + r, end).replaceAll('\n', ' ');
        if (code.length > 2 &&
            code[0] == ' ' &&
            code[code.length - 1] == ' ' &&
            _jsTrim(code).isNotEmpty) {
          code = code.substring(1, code.length - 1);
        }
        emit(HfyMdCode(code));
        i = end + r;
        continue;
      }
      if (!noLink && c == _lbracket) {
        final link = _link(i, depth);
        if (link != null) {
          emit(link.node);
          i = link.end;
          continue;
        }
      }
      if (!noLink && c == _lt) {
        final m = _auto.matchAsPrefix(s, i);
        if (m != null) {
          final href = hfySafeHref(m.group(1)!);
          if (href != null) {
            emit(HfyMdLink(href, [HfyMdText(m.group(1)!)]));
            i += m.group(0)!.length;
            continue;
          }
        }
      }
      if (!noLink &&
          (c == _h || c == _upperH) &&
          (i == 0 || !_isBareBoundary(s.codeUnitAt(i - 1)))) {
        final m = _bare.matchAsPrefix(s, i);
        if (m != null) {
          final url = _trimUrl(m.group(0)!);
          final href = hfySafeHref(url);
          if (href != null) {
            emit(HfyMdLink(href, [HfyMdText(url)]));
            i += url.length;
            continue;
          }
        }
      }
      if (c == _star || c == _underscore) {
        final em = _emphasis(i, depth, noLink);
        if (em != null) {
          emit(em.node);
          i = em.end;
          continue;
        }
        final r = _run(i, c);
        buf.write(s.substring(i, i + r));
        i += r;
        continue;
      }
      buf.writeCharCode(c);
      i++;
    }
    if (buf.isNotEmpty) out.add(HfyMdText(buf.toString()));
    // The scan is over: the memo goes before any child is parsed, so only one
    // memo is alive at a time however deeply the emphasis nests.
    _rowOf = null;
    _memo = null;
    _passed = null;
    return [
      for (final item in out)
        if (item is _Deferred)
          item.wrap(
            _Inline(
              s.substring(item.from, item.to),
            ).parse(depth + 1, item.noLink),
          )
        else
          item as HfyMdInline,
    ];
  }

  int _run(int i, int ch) {
    var k = 0;
    while (i + k < n && s.codeUnitAt(i + k) == ch) {
      k++;
    }
    return k;
  }

  // The first run of exactly `len` `ch` at or after `from`, scanning run by run
  // (ui.js:442-450). -1 when there is none.
  int _findRun(int from, int ch, int len) {
    var j = from;
    while (j < n) {
      if (s.codeUnitAt(j) != ch) {
        j++;
        continue;
      }
      final r = _run(j, ch);
      if (r == len) return j;
      j += r;
    }
    return -1;
  }

  ({Object node, int end})? _emphasis(int i, int depth, bool noLink) {
    final ch = s.codeUnitAt(i);
    final run = _run(i, ch);
    final after = i + run;
    if (run > 3 || after >= n || _isSpaceUnit(s.codeUnitAt(after))) return null;
    if (ch == _underscore && i > 0 && _isWordUnit(s.codeUnitAt(i - 1))) {
      return null;
    }
    final at = _closer(ch, after, run, depth);
    if (at == -1 || at <= after) return null;
    final HfyMdInline Function(List<HfyMdInline>) wrap = switch (run) {
      2 => HfyMdStrong.new,
      3 => (kids) => HfyMdStrong([HfyMdEmphasis(kids)]),
      _ => HfyMdEmphasis.new,
    };
    return (node: _Deferred(after, at, noLink, wrap), end: at + run);
  }

  // `hfyMdCloser` (ui.js:579-597): the forward scan for a closing run of `k`
  // delimiters `ch` from `from`. Returns the closing run's start, or -1.
  //
  // The walk is a function of its position alone (given `ch`, `k`, `depth`),
  // so every delimiter it passes is memoised with the answer of the walk from
  // there: a later walk that reaches the delimiter stops at once. Only
  // delimiters get memo rows, so the memo grows with them, not with the text.
  int _closer(int ch, int from, int k, int depth) {
    if (_memo == null) _allocMemo();
    final rowOf = _rowOf!;
    final memo = _memo!;
    final base = _passedCount;
    var found = -1;
    var j = from;
    while (j < n) {
      final c = s.codeUnitAt(j);
      if (c == ch) {
        final slot = rowOf[j] * 21 + (k - 1) * 7 + depth;
        final known = memo[slot];
        if (known != 0) {
          found = known - 2;
          break;
        }
        _pass(slot);
      }
      if (c == _backslash) {
        j += 2;
        continue;
      }
      if (c == _backtick) {
        final r = _run(j, _backtick);
        final e = _findRun(j + r, _backtick, r);
        j = e == -1 ? j + r : e + r;
        continue;
      }
      if (c != ch) {
        j++;
        continue;
      }
      final m = _run(j, ch);
      final prev = s.codeUnitAt(j - 1);
      final next = j + m < n ? s.codeUnitAt(j + m) : -1;
      final canClose =
          !_isSpaceUnit(prev) &&
          (ch != _underscore || next == -1 || !_isWordUnit(next));
      if (canClose && m == k) {
        found = j;
        break;
      }
      if (canClose && m == 3 && k < 3) {
        found = j + m - k;
        break;
      }
      final canOpen =
          next != -1 &&
          !_isSpaceUnit(next) &&
          m <= 3 &&
          (ch != _underscore || !_isWordUnit(prev));
      if (canOpen && depth < hfyMdDepthLimit) {
        final sub = _closer(ch, j + m, m, depth + 1);
        if (sub != -1) {
          j = sub + m;
          continue;
        }
      }
      j += m;
    }
    for (var q = base; q < _passedCount; q++) {
      memo[_passed![q]] = found + 2;
    }
    _passedCount = base;
    return found;
  }

  void _pass(int slot) {
    var passed = _passed!;
    if (_passedCount == passed.length) {
      passed = _passed = Int32List(passed.length * 2)..setAll(0, passed);
    }
    passed[_passedCount++] = slot;
  }

  // The row of the delimiter at j holds 21 slots, `(k - 1) * 7 + depth`, each
  // storing `closer + 2` (0: not computed yet).
  void _allocMemo() {
    final rowOf = Int32List(n);
    var rows = 0;
    for (var j = 0; j < n; j++) {
      final c = s.codeUnitAt(j);
      if (c == _star || c == _underscore) rowOf[j] = rows++;
    }
    _rowOf = rowOf;
    _memo = Int32List(rows * 21);
    _passed = Int32List(256);
  }

  ({Object node, int end})? _link(int i, int depth) {
    var d = 0;
    var j = i;
    final lim = min(n, i + 1000);
    for (; j < lim; j++) {
      final c = s.codeUnitAt(j);
      if (c == _backslash) {
        j++;
        continue;
      }
      if (c == _lbracket) {
        d++;
      } else if (c == _rbracket && --d == 0) {
        break;
      }
    }
    if (j >= lim || j + 1 >= n || s.codeUnitAt(j + 1) != _lparen) return null;
    var k = j + 2;
    var p = 1;
    final klim = min(n, k + 2048);
    for (; k < klim; k++) {
      final c = s.codeUnitAt(k);
      if (c == _backslash) {
        k++;
        continue;
      }
      if (c == _lf) return null;
      if (c == _lparen) {
        p++;
      } else if (c == _rparen && --p == 0) {
        break;
      }
    }
    if (p != 0 || k >= klim) return null;
    final dest = _jsTrim(s.substring(j + 2, k));
    final mm = _destAngle.firstMatch(dest) ?? _destPlain.firstMatch(dest);
    final href = mm == null ? null : hfySafeHref(mm.group(1)!);
    if (href == null) {
      return (node: HfyMdText(s.substring(i, k + 1)), end: k + 1);
    }
    final node = _Deferred(i + 1, j, true, (kids) => HfyMdLink(href, kids));
    return (node: node, end: k + 1);
  }
}

// A node whose children are the inline parse of `s[from, to)`, made after the
// scan (see [_Inline.parse]).
final class _Deferred {
  const _Deferred(this.from, this.to, this.noLink, this.wrap);

  final int from;
  final int to;
  final bool noLink;
  final HfyMdInline Function(List<HfyMdInline>) wrap;
}
