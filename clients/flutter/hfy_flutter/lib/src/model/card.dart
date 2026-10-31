// Tool cards: the server's `hfy/ui` block (ui.js:328-415). The fold stores
// the block verbatim (`tool_result.data.ui` / `approval.requested.data.ui`,
// ui.js:1011/1026); this module holds the one decision the FOLD itself makes
// about card shapes — which cards would render — because a tool block with an
// unusable/absent card still matters for the hidden-mode visibility rule
// (P-FOLD-024) while the DOM/Flutter building of the cards is the UI phase's
// (P-CRD-001: hfyCardEl/hfyCardRender are out of scope here).

/// One renderable card (ui.js:334 filter: a card draws when it has a title OR
/// a body). Kept as the raw map verbatim; rendering (Phase 3) reads fields.
class HfyCard {
  final Map<String, Object?> raw;

  const HfyCard(this.raw);

  Object? get title => raw['title'];
  Object? get body => raw['body'];
  Object? get badge => raw['badge'];
  Object? get subtitle => raw['subtitle'];
  Object? get image => raw['image'];
  List<Object?>? get kv =>
      raw['kv'] is List ? List<Object?>.from(raw['kv'] as List) : null;
  List<Object?>? get buttons => raw['buttons'] is List
      ? List<Object?>.from(raw['buttons'] as List)
      : null;
}

/// `hfyCardData` (ui.js:329-336): a `carousel` renders its `cards` array, a
/// single `card` renders itself; cards without title/body are dropped; an
/// empty result (or not-a-card) is null.
List<HfyCard>? hfyCardData(Object? raw) {
  if (raw is! Map<String, Object?>) return null;
  Iterable<Object?>? cards;
  if (raw['type'] == 'carousel' && raw['cards'] is List) {
    cards = List<Object?>.from(raw['cards'] as List);
  } else if (raw['type'] == 'card') {
    cards = [raw];
  }
  if (cards == null) return null;
  final out = <HfyCard>[];
  for (final c in cards) {
    if (c is! Map<String, Object?>) continue; // `c && typeof c === 'object'`
    if (_truthy(c['title']) || _truthy(c['body'])) out.add(HfyCard(c));
  }
  return out.isEmpty ? null : out;
}

// Local truthiness copy (see lib/src/model/values.dart's rationale): '' and 0
// do not count, but an object value does.
bool _truthy(Object? v) {
  if (v == null) return false;
  if (v is bool) return v;
  if (v is String) return v.isNotEmpty;
  if (v is num) return v != 0;
  return true;
}
