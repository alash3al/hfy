// The markdown tree (PARITY P-MD-001..003, P-MD-007, P-MD-010..012): the same
// nodes `hfyMdParse` builds in ui.js (hfyMdBlocks / hfyMdInline, ui.js:459-629),
// field for field. [toJson] emits the shape of clients/fixtures/markdown.json
// (`t` kind, `c` children, `v` text, `level`, `lang`, `text`, `start`, `items`,
// `href`); the fixture golden test compares that map, key order aside.

/// A block node (`p`, `h`, `pre`, `hr`, `quote`, `ul`, `ol`).
sealed class HfyMdBlock {
  const HfyMdBlock();

  Map<String, Object?> toJson();
}

/// `{t:'p', c}`: a paragraph of inline nodes.
final class HfyMdParagraph extends HfyMdBlock {
  final List<HfyMdInline> children;

  HfyMdParagraph(List<HfyMdInline> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'p',
    'c': [for (final n in children) n.toJson()],
  };
}

/// `{t:'h', level, c}`: a heading; `level` is `min(hashes, 3)` (ui.js:477).
final class HfyMdHeading extends HfyMdBlock {
  final int level;
  final List<HfyMdInline> children;

  HfyMdHeading(this.level, List<HfyMdInline> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'h',
    'level': level,
    'c': [for (final n in children) n.toJson()],
  };
}

/// `{t:'pre', lang, text}`: a fenced code block, `text` already dedented.
final class HfyMdCodeBlock extends HfyMdBlock {
  final String lang;
  final String text;

  const HfyMdCodeBlock({required this.lang, required this.text});

  @override
  Map<String, Object?> toJson() => {'t': 'pre', 'lang': lang, 'text': text};
}

/// `{t:'hr'}`: a thematic break.
final class HfyMdRule extends HfyMdBlock {
  const HfyMdRule();

  @override
  Map<String, Object?> toJson() => {'t': 'hr'};
}

/// `{t:'quote', c}`: a blockquote of nested blocks.
final class HfyMdQuote extends HfyMdBlock {
  final List<HfyMdBlock> children;

  HfyMdQuote(List<HfyMdBlock> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'quote',
    'c': [for (final b in children) b.toJson()],
  };
}

/// `{t:'ul'|'ol', start, items}`: a list. Bullets carry `start: 1` (ui.js:516).
final class HfyMdList extends HfyMdBlock {
  final bool ordered;
  final int start;
  final List<List<HfyMdBlock>> items;

  HfyMdList({
    required this.ordered,
    required this.start,
    required List<List<HfyMdBlock>> items,
  }) : items = List.unmodifiable([
         for (final item in items) List<HfyMdBlock>.unmodifiable(item),
       ]);

  @override
  Map<String, Object?> toJson() => {
    't': ordered ? 'ol' : 'ul',
    'start': start,
    'items': [
      for (final item in items) [for (final b in item) b.toJson()],
    ],
  };
}

/// A inline node (`text`, `br`, `code`, `b`, `i`, `a`).
sealed class HfyMdInline {
  const HfyMdInline();

  Map<String, Object?> toJson();
}

/// `{t:'text', v}`: literal text. Adjacent runs are not merged (ui.js:528).
final class HfyMdText extends HfyMdInline {
  final String value;

  const HfyMdText(this.value);

  @override
  Map<String, Object?> toJson() => {'t': 'text', 'v': value};
}

/// `{t:'br'}`: a newline inside a paragraph.
final class HfyMdLineBreak extends HfyMdInline {
  const HfyMdLineBreak();

  @override
  Map<String, Object?> toJson() => {'t': 'br'};
}

/// `{t:'code', v}`: an inline code span.
final class HfyMdCode extends HfyMdInline {
  final String value;

  const HfyMdCode(this.value);

  @override
  Map<String, Object?> toJson() => {'t': 'code', 'v': value};
}

/// `{t:'b', c}`: strong (`**x**`, or the outer half of `***x***`).
final class HfyMdStrong extends HfyMdInline {
  final List<HfyMdInline> children;

  HfyMdStrong(List<HfyMdInline> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'b',
    'c': [for (final n in children) n.toJson()],
  };
}

/// `{t:'i', c}`: emphasis (`*x*`, `_x_`).
final class HfyMdEmphasis extends HfyMdInline {
  final List<HfyMdInline> children;

  HfyMdEmphasis(List<HfyMdInline> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'i',
    'c': [for (final n in children) n.toJson()],
  };
}

/// `{t:'a', href, c}`: a link whose `href` already passed the safe-href rule.
final class HfyMdLink extends HfyMdInline {
  final String href;
  final List<HfyMdInline> children;

  HfyMdLink(this.href, List<HfyMdInline> children)
    : children = List.unmodifiable(children);

  @override
  Map<String, Object?> toJson() => {
    't': 'a',
    'href': href,
    'c': [for (final n in children) n.toJson()],
  };
}
