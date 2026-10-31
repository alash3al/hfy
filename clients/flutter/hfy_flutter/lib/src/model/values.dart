// JS value semantics, shared by the pure fold modules. The fold is a verbatim
// port of ui.js, so its booleans follow JS truthiness and string coercion —
// these tiny helpers keep every branch honest about that ({} and [] are
// truthy, '' and 0 are not; `a` without `b`'s falsiness wins).

import 'dart:convert' show jsonDecode;

/// JS `x` in a boolean position: null/false/0/''/^ are falsy, everything else
/// (including `{}` and `[]`) is truthy.
bool hfyTruthy(Object? v) {
  if (v == null) return false;
  if (v is bool) return v;
  if (v is String) return v.isNotEmpty;
  if (v is num) return v != 0;
  return true; // Map, List and anything else: JS only falsifies those above.
}

/// JS `a || b` over decoded values.
Object? hfyOr(Object? a, Object? b) => hfyTruthy(a) ? a : b;

/// JS `String(x)` for labels and stamps: null/undefined → '', everything else
/// through interpolation (numbers, bools, nested JSON print via toString).
String hfyString(Object? v) {
  if (v == null) return '';
  if (v is String) return v;
  return '$v';
}

/// The first truthy member of a JS `a || b || c` chain (for label fallbacks).
Object? hfyFirst(Object? a, [Object? b, Object? c]) {
  if (hfyTruthy(a)) return a;
  if (hfyTruthy(b)) return b;
  return c;
}

/// ui.js `hfyData` over one already-decoded value (ui.js:113-117): a JSON
/// string is parsed ({} on failure), anything empty → {}. [HfyEvent.fromJson]
/// applies the same rule to envelope `data`; this one re-applies it to
/// embedded payloads like an ask's `content`.
Object? hfyDataLike(Object? x) {
  if (x == null) return const <String, dynamic>{};
  if (x is String) {
    if (x.isEmpty) return const <String, dynamic>{};
    try {
      return jsonDecode(x);
    } on FormatException {
      return const <String, dynamic>{};
    }
  }
  return x;
}
