// Link safety for the markdown renderer and the tool cards (PARITY P-MD-011,
// P-CRD-004). Pure Dart, a port of ui.js:301-311: the browser's `new URL`
// becomes [parseHfyUrl], and `location.href` becomes an injected base [Uri].

import 'url.dart';

final RegExp _markdownHrefPrefix = RegExp(
  r'^(?:https?:\/\/|mailto:)',
  caseSensitive: false,
);

/// `hfySafeHref` (ui.js:302-306): a markdown link destination. Only absolute
/// http(s) and mailto URLs pass, returned in their serialized form (so
/// `https://x.example` comes back as `https://x.example/`). Anything else,
/// relative or unparsable included, is null and stays literal text.
String? hfySafeHref(String raw) {
  if (!_markdownHrefPrefix.hasMatch(raw)) return null;
  final url = parseHfyUrl(raw);
  if (url == null) return null;
  return _isHrefScheme(url.scheme) ? url.href : null;
}

/// `hfySafeUrl` (ui.js:308-311): a card or attachment URL, possibly relative
/// (the server's `/v1/files/...`), resolved against [base] (the page URL).
/// Only http(s) results pass; everything else is the empty string.
String hfySafeUrl(String? raw, Uri base) {
  if (raw == null || raw.isEmpty) return '';
  final origin = parseHfyUrl(base.toString());
  if (origin == null) return '';
  final url = parseHfyUrl(raw, base: origin);
  if (url == null) return '';
  return url.scheme == 'http' || url.scheme == 'https' ? url.href : '';
}

bool _isHrefScheme(String scheme) =>
    scheme == 'http' || scheme == 'https' || scheme == 'mailto';
