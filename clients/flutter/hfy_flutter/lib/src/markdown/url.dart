// WHATWG URL parsing for the markdown link policy (PARITY P-MD-011, P-MD-012;
// ui.js:301-311 relies on the browser's `new URL`). A transcription of the URL
// standard's basic URL parser, for the schemes the embed can ever show: http,
// https and mailto, plus relative references resolved against an http(s) base.
// Two parts are reduced, and only these:
//   - IDNA (the UTS #46 table is not reproduced): see [_domainToAscii];
//   - the other special schemes (ws, wss, ftp, file) parse as non-special. The
//     link policy rejects every scheme but http, https and mailto, so the
//     caller's answer is the same; a file: base is reported as failure.

import 'dart:convert' show utf8;

const int _eof = -1;

/// A URL record (WHATWG "URL record"). Filled in by [parseHfyUrl]; read back
/// through [href] and [scheme].
final class HfyUrl {
  String scheme = '';
  String username = '';
  String password = '';
  String? host;
  int? port;
  List<String> path = <String>[];
  String? opaquePath;
  String? query;
  String? fragment;

  /// The serialized URL (WHATWG "URL serializer", fragment included).
  String get href {
    final out = StringBuffer('$scheme:');
    final h = host;
    if (h != null) {
      out.write('//');
      if (username.isNotEmpty || password.isNotEmpty) {
        out.write(username);
        if (password.isNotEmpty) out.write(':$password');
        out.write('@');
      }
      out.write(h);
      if (port != null) out.write(':$port');
    }
    final op = opaquePath;
    if (h == null && op == null && path.length > 1 && path[0].isEmpty) {
      out.write('/.');
    }
    if (op != null) {
      out.write(op);
    } else {
      for (final seg in path) {
        out.write('/');
        out.write(seg);
      }
    }
    if (query != null) out.write('?$query');
    if (fragment != null) out.write('#$fragment');
    return out.toString();
  }
}

enum _State {
  schemeStart,
  scheme,
  noScheme,
  specialRelativeOrAuthority,
  pathOrAuthority,
  relative,
  relativeSlash,
  specialAuthoritySlashes,
  specialAuthorityIgnoreSlashes,
  authority,
  host,
  port,
  pathStart,
  path,
  opaquePath,
  query,
  fragment,
}

/// The basic URL parser (WHATWG URL standard, no state override). Returns
/// null wherever the standard reports failure. [base] is an already parsed
/// absolute URL, as `new URL(input, base)` takes.
HfyUrl? parseHfyUrl(String input, {HfyUrl? base}) {
  final cps = _usvCodePoints(input);
  final n = cps.length;
  int at(int i) => i < n ? cps[i] : _eof;
  // Only http and https are special here (see the header on the other schemes).
  bool special(HfyUrl u) => u.scheme == 'http' || u.scheme == 'https';

  final url = HfyUrl();
  var state = _State.schemeStart;
  final buf = StringBuffer();
  var atSignSeen = false;
  var insideBrackets = false;
  var passwordTokenSeen = false;
  var p = 0;

  while (true) {
    final c = at(p);
    switch (state) {
      case _State.schemeStart:
        if (_isAsciiAlpha(c)) {
          buf.write(_cpString(_lowerAscii(c)));
          state = _State.scheme;
        } else {
          state = _State.noScheme;
          p--;
        }
      case _State.scheme:
        if (_isAsciiAlpha(c) ||
            _isAsciiDigit(c) ||
            c == 0x2B ||
            c == 0x2D ||
            c == 0x2E) {
          buf.write(_cpString(_lowerAscii(c)));
        } else if (c == 0x3A) {
          url.scheme = buf.toString();
          buf.clear();
          if (special(url)) {
            state = base != null && base.scheme == url.scheme
                ? _State.specialRelativeOrAuthority
                : _State.specialAuthoritySlashes;
          } else if (at(p + 1) == 0x2F) {
            state = _State.pathOrAuthority;
            p++;
          } else {
            url.opaquePath = '';
            state = _State.opaquePath;
          }
        } else {
          buf.clear();
          state = _State.noScheme;
          p = -1;
        }
      case _State.noScheme:
        if (base == null || (base.opaquePath != null && c != 0x23)) return null;
        if (base.opaquePath != null) {
          url.scheme = base.scheme;
          url.opaquePath = base.opaquePath;
          url.query = base.query;
          url.fragment = '';
          state = _State.fragment;
        } else if (base.scheme == 'file') {
          return null;
        } else {
          state = _State.relative;
          p--;
        }
      case _State.specialRelativeOrAuthority:
        if (c == 0x2F && at(p + 1) == 0x2F) {
          state = _State.specialAuthorityIgnoreSlashes;
          p++;
        } else {
          state = _State.relative;
          p--;
        }
      case _State.pathOrAuthority:
        if (c == 0x2F) {
          state = _State.authority;
        } else {
          state = _State.path;
          p--;
        }
      case _State.relative:
        final b = base!;
        url.scheme = b.scheme;
        if (c == 0x2F || (special(url) && c == 0x5C)) {
          state = _State.relativeSlash;
        } else {
          url.username = b.username;
          url.password = b.password;
          url.host = b.host;
          url.port = b.port;
          url.path = List<String>.of(b.path);
          url.query = b.query;
          if (c == 0x3F) {
            url.query = '';
            state = _State.query;
          } else if (c == 0x23) {
            url.fragment = '';
            state = _State.fragment;
          } else if (c != _eof) {
            url.query = null;
            _shorten(url);
            state = _State.path;
            p--;
          }
        }
      case _State.relativeSlash:
        final b = base!;
        if (special(url) && (c == 0x2F || c == 0x5C)) {
          state = _State.specialAuthorityIgnoreSlashes;
        } else if (c == 0x2F) {
          state = _State.authority;
        } else {
          url.username = b.username;
          url.password = b.password;
          url.host = b.host;
          url.port = b.port;
          state = _State.path;
          p--;
        }
      case _State.specialAuthoritySlashes:
        if (c == 0x2F && at(p + 1) == 0x2F) {
          state = _State.specialAuthorityIgnoreSlashes;
          p++;
        } else {
          state = _State.specialAuthorityIgnoreSlashes;
          p--;
        }
      case _State.specialAuthorityIgnoreSlashes:
        if (c != 0x2F && c != 0x5C) {
          state = _State.authority;
          p--;
        }
      case _State.authority:
        if (c == 0x40) {
          if (atSignSeen) {
            final prior = buf.toString();
            buf.clear();
            buf.write('%40$prior');
          }
          atSignSeen = true;
          for (final cp in buf.toString().runes) {
            if (cp == 0x3A && !passwordTokenSeen) {
              passwordTokenSeen = true;
              continue;
            }
            final enc = _pct(cp, _isUserinfo);
            if (passwordTokenSeen) {
              url.password += enc;
            } else {
              url.username += enc;
            }
          }
          buf.clear();
        } else if (c == _eof ||
            c == 0x2F ||
            c == 0x3F ||
            c == 0x23 ||
            (special(url) && c == 0x5C)) {
          if (atSignSeen && buf.isEmpty) return null;
          p -= buf.toString().runes.length + 1;
          buf.clear();
          state = _State.host;
        } else {
          buf.write(_cpString(c));
        }
      case _State.host:
        if (c == 0x3A && !insideBrackets) {
          if (buf.isEmpty) return null;
          final h = _host(buf.toString(), opaque: !special(url));
          if (h == null) return null;
          url.host = h;
          buf.clear();
          state = _State.port;
        } else if (c == _eof ||
            c == 0x2F ||
            c == 0x3F ||
            c == 0x23 ||
            (special(url) && c == 0x5C)) {
          p--;
          if (special(url) && buf.isEmpty) return null;
          final h = _host(buf.toString(), opaque: !special(url));
          if (h == null) return null;
          url.host = h;
          buf.clear();
          state = _State.pathStart;
        } else {
          if (c == 0x5B) insideBrackets = true;
          if (c == 0x5D) insideBrackets = false;
          buf.write(_cpString(c));
        }
      case _State.port:
        if (_isAsciiDigit(c)) {
          buf.write(_cpString(c));
        } else if (c == _eof ||
            c == 0x2F ||
            c == 0x3F ||
            c == 0x23 ||
            (special(url) && c == 0x5C)) {
          if (buf.isNotEmpty) {
            final v = _decimalPort(buf.toString());
            if (v == null) return null;
            url.port = v == _defaultPort(url.scheme) ? null : v;
            buf.clear();
          }
          state = _State.pathStart;
          p--;
        } else {
          return null;
        }
      case _State.pathStart:
        if (special(url)) {
          state = _State.path;
          if (c != 0x2F && c != 0x5C) p--;
        } else if (c == 0x3F) {
          url.query = '';
          state = _State.query;
        } else if (c == 0x23) {
          url.fragment = '';
          state = _State.fragment;
        } else if (c != _eof) {
          state = _State.path;
          if (c != 0x2F) p--;
        }
      case _State.path:
        final slash = c == 0x2F || (special(url) && c == 0x5C);
        if (c == _eof || slash || c == 0x3F || c == 0x23) {
          final segment = buf.toString();
          if (_isDoubleDot(segment)) {
            _shorten(url);
            if (!slash) url.path.add('');
          } else if (_isSingleDot(segment)) {
            if (!slash) url.path.add('');
          } else {
            url.path.add(segment);
          }
          buf.clear();
          if (c == 0x3F) {
            url.query = '';
            state = _State.query;
          } else if (c == 0x23) {
            url.fragment = '';
            state = _State.fragment;
          }
        } else {
          buf.write(_pct(c, _isPath));
        }
      case _State.opaquePath:
        if (c == 0x3F) {
          url.query = '';
          state = _State.query;
        } else if (c == 0x23) {
          url.fragment = '';
          state = _State.fragment;
        } else if (c == 0x20) {
          final next = at(p + 1);
          url.opaquePath =
              '${url.opaquePath}${next == 0x3F || next == 0x23 ? '%20' : ' '}';
        } else if (c != _eof) {
          url.opaquePath = '${url.opaquePath}${_pct(c, _isC0)}';
        }
      case _State.query:
        if (c == 0x23 || c == _eof) {
          final inSet = special(url) ? _isSpecialQuery : _isQuery;
          url.query = (url.query ?? '') + _pctAll(buf.toString(), inSet);
          buf.clear();
          if (c == 0x23) {
            url.fragment = '';
            state = _State.fragment;
          }
        } else {
          buf.write(_cpString(c));
        }
      case _State.fragment:
        if (c != _eof) {
          url.fragment = (url.fragment ?? '') + _pct(c, _isFragment);
        }
    }
    if (p >= n) break;
    p++;
  }
  return url;
}

// Leading and trailing C0 controls and spaces go, tabs and newlines vanish,
// and lone surrogates become U+FFFD (the USVString step of the constructor).
List<int> _usvCodePoints(String input) {
  var start = 0;
  var end = input.length;
  while (start < end && input.codeUnitAt(start) <= 0x20) {
    start++;
  }
  while (end > start && input.codeUnitAt(end - 1) <= 0x20) {
    end--;
  }
  final out = <int>[];
  for (final cp in input.substring(start, end).runes) {
    if (cp == 0x09 || cp == 0x0A || cp == 0x0D) continue;
    out.add(cp >= 0xD800 && cp <= 0xDFFF ? 0xFFFD : cp);
  }
  return out;
}

void _shorten(HfyUrl url) {
  if (url.path.isNotEmpty) url.path.removeLast();
}

int? _defaultPort(String scheme) => switch (scheme) {
  'http' => 80,
  'https' => 443,
  _ => null,
};

int? _decimalPort(String digits) {
  var v = 0;
  for (var i = 0; i < digits.length; i++) {
    v = v * 10 + (digits.codeUnitAt(i) - 0x30);
    if (v > 65535) return null;
  }
  return v;
}

bool _isDoubleDot(String s) {
  final l = s.toLowerCase();
  return l == '..' || l == '.%2e' || l == '%2e.' || l == '%2e%2e';
}

bool _isSingleDot(String s) {
  final l = s.toLowerCase();
  return l == '.' || l == '%2e';
}

// ---- hosts ----

/// The host parser (WHATWG "host parser"); null on failure. [opaque] is true
/// for non-special schemes, where the host is not a domain.
String? _host(String input, {required bool opaque}) {
  if (input.startsWith('[')) {
    if (!input.endsWith(']')) return null;
    final v6 = _ipv6(input.substring(1, input.length - 1));
    return v6 == null ? null : '[${_ipv6Serialize(v6)}]';
  }
  if (opaque) {
    for (final cp in input.runes) {
      if (_isForbiddenHost(cp)) return null;
    }
    return _pctAll(input, _isC0);
  }
  final ascii = _domainToAscii(_percentDecodeUtf8(input));
  if (ascii == null) return null;
  for (final cu in ascii.codeUnits) {
    if (_isForbiddenDomain(cu)) return null;
  }
  if (_endsInNumber(ascii)) return _ipv4(ascii);
  return ascii;
}

bool _isForbiddenHost(int cp) =>
    cp == 0x00 ||
    cp == 0x09 ||
    cp == 0x0A ||
    cp == 0x0D ||
    cp == 0x20 ||
    cp == 0x23 ||
    cp == 0x2F ||
    cp == 0x3A ||
    cp == 0x3C ||
    cp == 0x3E ||
    cp == 0x3F ||
    cp == 0x40 ||
    cp == 0x5B ||
    cp == 0x5C ||
    cp == 0x5D ||
    cp == 0x5E ||
    cp == 0x7C;

bool _isForbiddenDomain(int cp) =>
    _isForbiddenHost(cp) || cp <= 0x1F || cp == 0x25 || cp == 0x7F;

String _percentDecodeUtf8(String input) {
  final src = <int>[for (final cp in input.runes) ..._utf8Bytes(cp)];
  final bytes = <int>[];
  for (var i = 0; i < src.length; i++) {
    if (src[i] == 0x25 &&
        i + 2 < src.length &&
        _hexValue(src[i + 1]) >= 0 &&
        _hexValue(src[i + 2]) >= 0) {
      bytes.add(_hexValue(src[i + 1]) * 16 + _hexValue(src[i + 2]));
      i += 2;
    } else {
      bytes.add(src[i]);
    }
  }
  return utf8.decode(bytes, allowMalformed: true);
}

// shortcut: UTS #46 IDNA reduced to what the link policy meets. Mapped: ASCII
// and fullwidth ASCII to lowercase ASCII, the ideographic full stops to '.',
// U+0130 to "i" + combining dot, the Latin compatibility letters in
// [_latinCompat], other letters through Dart's lowercase. Ignored code points
// drop out. Rejected: controls, unassigned, private use, surrogates, space
// separators, U+FFFD. Not reproduced: the rest of the NFKC compatibility
// mapping (superscripts, mathematical alphabets, ...), NFC, the bidi and joiner
// label rules, and the rest of the UTS #46 disallowed table. Upgrade with the
// IdnaMappingTable when a non-ASCII host ever reaches the policy in practice.
const Map<int, String> _latinCompat = {
  0x17F: 's',
  0x1C4: 'd\u017E',
  0x1C5: 'd\u017E',
  0x1C6: 'd\u017E',
  0x1C7: 'lj',
  0x1C8: 'lj',
  0x1C9: 'lj',
  0x1CA: 'nj',
  0x1CB: 'nj',
  0x1CC: 'nj',
  0x1F1: 'dz',
  0x1F2: 'dz',
  0x1F3: 'dz',
  0xFB00: 'ff',
  0xFB01: 'fi',
  0xFB02: 'fl',
  0xFB03: 'ffi',
  0xFB04: 'ffl',
  0xFB05: 'st',
  0xFB06: 'st',
};

String? _domainToAscii(String domain) {
  final mapped = StringBuffer();
  for (final cp in domain.runes) {
    if (_isIgnoredCodePoint(cp)) continue;
    final compat = _latinCompat[cp];
    if (compat != null) {
      mapped.write(compat);
    } else if (cp == 0x3002 || cp == 0xFF0E || cp == 0xFF61) {
      mapped.write('.');
    } else if (cp >= 0xFF01 && cp <= 0xFF5E) {
      mapped.writeCharCode(_lowerAscii(cp - 0xFEE0));
    } else if (cp < 0x80) {
      mapped.writeCharCode(_lowerAscii(cp));
    } else {
      if (_isRejectedCodePoint(cp)) return null;
      mapped.write(
        cp == 0x130 ? 'i\u0307' : String.fromCharCodes([cp]).toLowerCase(),
      );
    }
  }
  final labels = <String>[];
  for (final label in mapped.toString().split('.')) {
    if (label.codeUnits.every((cu) => cu < 0x80)) {
      if (label.startsWith('xn--') &&
          !_isCanonicalPunycode(label.substring(4))) {
        return null;
      }
      labels.add(label);
    } else {
      if (label.startsWith('xn--')) return null;
      labels.add('xn--${_punyEncode(label.runes.toList())}');
    }
  }
  final ascii = labels.join('.');
  return ascii.isEmpty ? null : ascii;
}

bool _isIgnoredCodePoint(int cp) =>
    cp == 0xAD ||
    cp == 0x34F ||
    (cp >= 0x180B && cp <= 0x180E) ||
    cp == 0x200B ||
    cp == 0x2060 ||
    (cp >= 0xFE00 && cp <= 0xFE0F) ||
    cp == 0xFEFF;

bool _isRejectedCodePoint(int cp) =>
    cp <= 0x1F ||
    (cp >= 0x7F && cp <= 0x9F) ||
    cp == 0x20 ||
    cp == 0xA0 ||
    cp == 0x1680 ||
    (cp >= 0x2000 && cp <= 0x200A) ||
    cp == 0x2028 ||
    cp == 0x2029 ||
    cp == 0x202F ||
    cp == 0x205F ||
    cp == 0x3000 ||
    cp == 0xFFFD ||
    (cp >= 0xD800 && cp <= 0xDFFF) ||
    (cp >= 0xE000 && cp <= 0xF8FF) ||
    (cp >= 0xFDD0 && cp <= 0xFDEF) ||
    (cp & 0xFFFE) == 0xFFFE ||
    _unassignedOrPrivate.hasMatch(String.fromCharCodes([cp]));

final RegExp _unassignedOrPrivate = RegExp(r'^[\p{Cn}\p{Co}]$', unicode: true);

bool _endsInNumber(String s) {
  var parts = s.split('.');
  if (parts.last.isEmpty) {
    if (parts.length == 1) return false;
    parts = parts.sublist(0, parts.length - 1);
  }
  final last = parts.last;
  if (last.isNotEmpty && _isAllAsciiDigits(last)) return true;
  return _ipv4Number(last) != null;
}

String? _ipv4(String s) {
  var parts = s.split('.');
  if (parts.last.isEmpty && parts.length > 1) {
    parts = parts.sublist(0, parts.length - 1);
  }
  if (parts.length > 4) return null;
  final numbers = <int>[];
  for (final part in parts) {
    final v = _ipv4Number(part);
    if (v == null) return null;
    numbers.add(v);
  }
  for (var i = 0; i < numbers.length - 1; i++) {
    if (numbers[i] > 255) return null;
  }
  var limit = 1;
  for (var i = 0; i < 5 - numbers.length; i++) {
    limit *= 256;
  }
  if (numbers.last >= limit) return null;
  var ipv4 = numbers.last;
  for (var i = 0; i < numbers.length - 1; i++) {
    var scale = 1;
    for (var k = 0; k < 3 - i; k++) {
      scale *= 256;
    }
    ipv4 += numbers[i] * scale;
  }
  return '${ipv4 >> 24 & 0xFF}.${ipv4 >> 16 & 0xFF}.${ipv4 >> 8 & 0xFF}.${ipv4 & 0xFF}';
}

// Values past 2^40 are clamped: every caller compares against at most 2^32, so
// the clamp changes no outcome and keeps the arithmetic in range.
int? _ipv4Number(String s) {
  if (s.isEmpty) return null;
  var radix = 10;
  var digits = s;
  if (s.length >= 2 && (s.startsWith('0x') || s.startsWith('0X'))) {
    radix = 16;
    digits = s.substring(2);
  } else if (s.length >= 2 && s.codeUnitAt(0) == 0x30) {
    radix = 8;
    digits = s.substring(1);
  }
  if (digits.isEmpty) return 0;
  var v = 0;
  for (var i = 0; i < digits.length; i++) {
    final d = _digitValue(digits.codeUnitAt(i));
    if (d < 0 || d >= radix) return null;
    v = v * radix + d;
    if (v > 0x10000000000) v = 0x10000000000;
  }
  return v;
}

int _digitValue(int cu) {
  if (cu >= 0x30 && cu <= 0x39) return cu - 0x30;
  final l = cu | 0x20;
  if (l >= 0x61 && l <= 0x66) return l - 0x61 + 10;
  return -1;
}

List<int>? _ipv6(String input) {
  final address = List<int>.filled(8, 0);
  final cs = input.codeUnits;
  final n = cs.length;
  int at(int i) => i < n ? cs[i] : _eof;
  var pieceIndex = 0;
  int? compress;
  var p = 0;
  if (at(p) == 0x3A) {
    if (at(p + 1) != 0x3A) return null;
    p += 2;
    pieceIndex++;
    compress = pieceIndex;
  }
  while (at(p) != _eof) {
    if (pieceIndex == 8) return null;
    if (at(p) == 0x3A) {
      if (compress != null) return null;
      p++;
      pieceIndex++;
      compress = pieceIndex;
      continue;
    }
    var value = 0;
    var length = 0;
    while (length < 4 && _hexValue(at(p)) >= 0) {
      value = value * 16 + _hexValue(at(p));
      p++;
      length++;
    }
    if (at(p) == 0x2E) {
      if (length == 0) return null;
      p -= length;
      if (pieceIndex > 6) return null;
      var numbersSeen = 0;
      while (at(p) != _eof) {
        int? ipv4Piece;
        if (numbersSeen > 0) {
          if (at(p) == 0x2E && numbersSeen < 4) {
            p++;
          } else {
            return null;
          }
        }
        if (!_isAsciiDigit(at(p))) return null;
        while (_isAsciiDigit(at(p))) {
          final number = at(p) - 0x30;
          if (ipv4Piece == null) {
            ipv4Piece = number;
          } else if (ipv4Piece == 0) {
            return null;
          } else {
            ipv4Piece = ipv4Piece * 10 + number;
          }
          if (ipv4Piece > 255) return null;
          p++;
        }
        address[pieceIndex] = address[pieceIndex] * 0x100 + ipv4Piece!;
        numbersSeen++;
        if (numbersSeen == 2 || numbersSeen == 4) pieceIndex++;
      }
      if (numbersSeen != 4) return null;
      break;
    } else if (at(p) == 0x3A) {
      p++;
      if (at(p) == _eof) return null;
    } else if (at(p) != _eof) {
      return null;
    }
    address[pieceIndex] = value;
    pieceIndex++;
  }
  if (compress != null) {
    var swaps = pieceIndex - compress;
    pieceIndex = 7;
    while (pieceIndex != 0 && swaps > 0) {
      final other = compress + swaps - 1;
      final tmp = address[pieceIndex];
      address[pieceIndex] = address[other];
      address[other] = tmp;
      pieceIndex--;
      swaps--;
    }
  } else if (pieceIndex != 8) {
    return null;
  }
  return address;
}

String _ipv6Serialize(List<int> address) {
  var compress = -1;
  var bestLength = 1;
  for (var i = 0; i < 8;) {
    if (address[i] != 0) {
      i++;
      continue;
    }
    var j = i;
    while (j < 8 && address[j] == 0) {
      j++;
    }
    if (j - i > bestLength) {
      bestLength = j - i;
      compress = i;
    }
    i = j;
  }
  final out = StringBuffer();
  var ignore0 = false;
  for (var i = 0; i < 8; i++) {
    if (ignore0 && address[i] == 0) continue;
    ignore0 = false;
    if (compress == i) {
      out.write(i == 0 ? '::' : ':');
      ignore0 = true;
      continue;
    }
    out.write(address[i].toRadixString(16));
    if (i != 7) out.write(':');
  }
  return out.toString();
}

// ---- punycode (RFC 3492) ----

const int _pBase = 36;
const int _pTMin = 1;
const int _pTMax = 26;
const int _pSkew = 38;
const int _pDamp = 700;
const int _pInitialBias = 72;
const int _pInitialN = 0x80;

int _adapt(int delta, int numPoints, bool firstTime) {
  var d = firstTime ? delta ~/ _pDamp : delta ~/ 2;
  d += d ~/ numPoints;
  var k = 0;
  while (d > ((_pBase - _pTMin) * _pTMax) ~/ 2) {
    d ~/= _pBase - _pTMin;
    k += _pBase;
  }
  return k + ((_pBase - _pTMin + 1) * d) ~/ (d + _pSkew);
}

int _threshold(int k, int bias) =>
    k <= bias ? _pTMin : (k >= bias + _pTMax ? _pTMax : k - bias);

String _punyEncode(List<int> input) {
  final out = StringBuffer();
  var basic = 0;
  for (final c in input) {
    if (c < 0x80) {
      out.writeCharCode(c);
      basic++;
    }
  }
  var handled = basic;
  if (basic > 0) out.write('-');
  var n = _pInitialN;
  var delta = 0;
  var bias = _pInitialBias;
  while (handled < input.length) {
    var m = 0x7FFFFFFF;
    for (final c in input) {
      if (c >= n && c < m) m = c;
    }
    delta += (m - n) * (handled + 1);
    n = m;
    for (final c in input) {
      if (c < n) delta++;
      if (c != n) continue;
      var q = delta;
      for (var k = _pBase; ; k += _pBase) {
        final t = _threshold(k, bias);
        if (q < t) break;
        out.writeCharCode(_punyDigit(t + (q - t) % (_pBase - t)));
        q = (q - t) ~/ (_pBase - t);
      }
      out.writeCharCode(_punyDigit(q));
      bias = _adapt(delta, handled + 1, handled == basic);
      delta = 0;
      handled++;
    }
    delta++;
    n++;
  }
  return out.toString();
}

int _punyDigit(int d) => d < 26 ? d + 0x61 : d - 26 + 0x30;

int _punyDecodeDigit(int cu) {
  if (cu >= 0x30 && cu <= 0x39) return cu - 0x30 + 26;
  final l = cu | 0x20;
  if (l >= 0x61 && l <= 0x7A) return l - 0x61;
  return -1;
}

List<int>? _punyDecode(String input) {
  final output = <int>[];
  final basic = input.lastIndexOf('-').clamp(0, input.length);
  for (var j = 0; j < basic; j++) {
    final c = input.codeUnitAt(j);
    if (c >= 0x80) return null;
    output.add(c);
  }
  var n = _pInitialN;
  var i = 0;
  var bias = _pInitialBias;
  var pos = basic > 0 ? basic + 1 : 0;
  while (pos < input.length) {
    final oldi = i;
    var w = 1;
    for (var k = _pBase; ; k += _pBase) {
      if (pos >= input.length) return null;
      final digit = _punyDecodeDigit(input.codeUnitAt(pos++));
      if (digit < 0) return null;
      i += digit * w;
      if (i > 0x7FFFFFFF) return null;
      final t = _threshold(k, bias);
      if (digit < t) break;
      w *= _pBase - t;
      if (w > 0x7FFFFFFF) return null;
    }
    bias = _adapt(i - oldi, output.length + 1, oldi == 0);
    n += i ~/ (output.length + 1);
    i %= output.length + 1;
    if (n > 0x10FFFF) return null;
    output.insert(i, n);
    i++;
  }
  return output;
}

// An "xn--" label is accepted only when it decodes to non-ASCII text that is
// itself accepted, and re-encodes to exactly the same label.
bool _isCanonicalPunycode(String encoded) {
  final decoded = _punyDecode(encoded);
  if (decoded == null || decoded.isEmpty) return false;
  if (decoded.every((cp) => cp < 0x80)) return false;
  for (final cp in decoded) {
    if (_isRejectedCodePoint(cp) || _isIgnoredCodePoint(cp)) return false;
  }
  return _punyEncode(decoded) == encoded;
}

// ---- percent-encoding (WHATWG "percent-encode sets") ----

List<int> _utf8Bytes(int cp) {
  if (cp < 0x80) return [cp];
  if (cp < 0x800) return [0xC0 | (cp >> 6), 0x80 | (cp & 0x3F)];
  if (cp < 0x10000) {
    return [0xE0 | (cp >> 12), 0x80 | ((cp >> 6) & 0x3F), 0x80 | (cp & 0x3F)];
  }
  return [
    0xF0 | (cp >> 18),
    0x80 | ((cp >> 12) & 0x3F),
    0x80 | ((cp >> 6) & 0x3F),
    0x80 | (cp & 0x3F),
  ];
}

const String _hexDigits = '0123456789ABCDEF';

String _pct(int cp, bool Function(int) inSet) {
  if (cp < 0x80 && !inSet(cp)) return _cpString(cp);
  final out = StringBuffer();
  for (final b in _utf8Bytes(cp)) {
    out.write('%');
    out.write(_hexDigits[b >> 4]);
    out.write(_hexDigits[b & 0xF]);
  }
  return out.toString();
}

String _pctAll(String s, bool Function(int) inSet) {
  final out = StringBuffer();
  for (final cp in s.runes) {
    out.write(_pct(cp, inSet));
  }
  return out.toString();
}

bool _isC0(int cp) => cp < 0x20 || cp > 0x7E;

bool _isFragment(int cp) =>
    _isC0(cp) ||
    cp == 0x20 ||
    cp == 0x22 ||
    cp == 0x3C ||
    cp == 0x3E ||
    cp == 0x60;

bool _isQuery(int cp) =>
    _isC0(cp) ||
    cp == 0x20 ||
    cp == 0x22 ||
    cp == 0x23 ||
    cp == 0x3C ||
    cp == 0x3E;

bool _isSpecialQuery(int cp) => _isQuery(cp) || cp == 0x27;

bool _isPath(int cp) =>
    _isQuery(cp) ||
    cp == 0x3F ||
    cp == 0x5E ||
    cp == 0x60 ||
    cp == 0x7B ||
    cp == 0x7D;

bool _isUserinfo(int cp) =>
    _isPath(cp) ||
    cp == 0x2F ||
    cp == 0x3A ||
    cp == 0x3B ||
    cp == 0x3D ||
    cp == 0x40 ||
    (cp >= 0x5B && cp <= 0x5D) ||
    cp == 0x7C;

// ---- small character helpers ----

bool _isAsciiAlpha(int c) =>
    (c >= 0x41 && c <= 0x5A) || (c >= 0x61 && c <= 0x7A);

bool _isAsciiDigit(int c) => c >= 0x30 && c <= 0x39;

bool _isAllAsciiDigits(String s) {
  for (var i = 0; i < s.length; i++) {
    if (!_isAsciiDigit(s.codeUnitAt(i))) return false;
  }
  return true;
}

int _hexValue(int c) {
  if (_isAsciiDigit(c)) return c - 0x30;
  final l = c | 0x20;
  if (l >= 0x61 && l <= 0x66) return l - 0x61 + 10;
  return -1;
}

int _lowerAscii(int c) => c >= 0x41 && c <= 0x5A ? c + 0x20 : c;

String _cpString(int cp) => String.fromCharCodes([cp]);
