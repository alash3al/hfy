// The review-fix pass, mechanical half: the controller-facing API (apply
// booleans, revision, cached turns, immutable views, hasOlder/fileLabel),
// the batched fold (applyAll == per-event apply for every shared fixture
// plus randomized orderings), the atomic mergeOlderPage (old two-step API
// removed) and a 10k-event smoke guard. Behavior-parity rules with PARITY
// IDs live in fold_parity_test.dart and fold_rules_test.dart.

import 'dart:convert' show jsonDecode, jsonEncode;
import 'dart:math' show Random;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/model/event.dart';
import 'package:hfy_flutter/src/model/fold.dart';
import 'package:hfy_flutter/src/model/state.dart';

import '../helpers/fixture_loader.dart' show fixtureFiles;
import '../support/fixture_harness.dart' show embedExample, runScenario;
import '../support/projection.dart' show firstJsonDifference;

// 2026-01-02 10:00-based UTC stamps, mirroring fold_rules_test.dart.
DateTime at(int min, [int s = 0]) => DateTime.utc(2026, 1, 2, 10, min, s);
const u1 = 'u1';
final userActor = HfyActor(uuid: u1, type: 'external', ref: u1);
final agentActor = HfyActor(uuid: 'agent', type: 'agent', ref: 'main');

HfyEvent ev(
  String id,
  String type, {
  String parent = 'turn-1',
  HfyActor? actor,
  Object? data,
  DateTime? when,
}) {
  return HfyEvent(
    id: id,
    type: type,
    parent: parent,
    actor: actor ?? userActor,
    data: data is Map<String, Object?> ? data : const <String, Object?>{},
    createdAt: when ?? at(0),
  );
}

HfyConversation foldOf({HfyToolCalls mode = HfyToolCalls.summary}) {
  final c = HfyConversation(
    viewer: const HfyViewer(uuid: u1, type: 'external', ref: u1),
    toolCalls: mode,
    base: embedExample,
    tz: const HfyTz.utc(),
  );
  c.markLoaded();
  return c;
}

extension _Find on HfyConversation {
  HfyTurn get t1 => turns.firstWhere((t) => t.id == 'turn-1');
}

/// A value whose toString throws — proves mergeOlderPage can fail mid-fold
/// (the fold itself never throws on wire-shaped data; garbage in an
/// attachments entry reaches the fold's String() coercion).
class _Shatter {
  @override
  String toString() => throw StateError('shatter');
}

/// Shuffles a deepcopy'd fixture def's main event sequence. Page order and
/// every other header key stay untouched (pages fold through mergeOlderPage
/// in both paths anyway — the batching claim only covers streamed events).
void shuffleEvents(Map<String, dynamic> def, Random rng) {
  final events = def['events'];
  if (events is! List) return;
  final list = <Object?>[...events]..shuffle(rng);
  def['events'] = list;
}

/// Build a deterministic ~10k-event history (one turn, all block kinds) for
/// the batch smoke guard.
List<HfyEvent> tenKEvents() {
  final events = <HfyEvent>[];
  events.add(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
  var i = 0;
  while (events.length < 10000) {
    events.add(ev('t$i', 'message.text', data: {'text': 'user text $i'}));
    events.add(ev('d$i', 'text.delta', actor: agentActor, data: {'text': 'chunk $i '}));
    events.add(ev('m$i', 'message.text', actor: agentActor, data: {'text': 'reply $i'}));
    events.add(
      ev('c$i', 'tool_call', actor: agentActor,
          data: {'name': 'files.read', 'call_id': 'c$i', 'arguments': {'n': i}}),
    );
    events.add(
      ev('r$i', 'tool_result', actor: agentActor,
          data: {'call_id': 'c$i', 'content': 'ok'}),
    );
    i++;
  }
  return events;
}

/// A seeded grammar over everything the sheet bookkeeping reacts to:
/// requests, decisions, interrupts and results for the SAME call ids
/// (known and ghost), terminal states, user-text starters and stream
/// appends. Returns flat events cut into chunks plus the per-boundary
/// user action ('min', 'open' or '').
(List<List<HfyEvent>>, List<String>) _randomChunks(Random rng) {
  final flat = <HfyEvent>[
    ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}),
    ev('b1', 'message.text', data: {'text': 'hello'}),
  ];
  const cids = ['c1', 'c2', 'c3', 'ghost'];
  const states = ['queued', 'running', 'completed', 'stopped'];
  for (var i = 0; i < 12 + rng.nextInt(14); i++) {
    final cid = cids[rng.nextInt(cids.length)];
    switch (rng.nextInt(9)) {
      case 0:
        flat.add(ev('u$i', 'message.text', data: {'text': 'more'}, when: at(i + 1)));
      case 1:
        flat.add(ev('d$i', 'text.delta', actor: agentActor,
            data: {'text': ' chunk '}, when: at(i + 1)));
      case 2:
        flat.add(ev('m$i', 'message.text', actor: agentActor,
            data: {'text': 'reply $i'}, when: at(i + 1)));
      case 3:
        flat.add(ev('c$i', 'tool_call', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': cid, 'arguments': {'n': i}},
            when: at(i + 1)));
      case 4:
        flat.add(ev('r$i', 'tool_result', actor: agentActor,
            data: {'call_id': cid, 'content': 'ok'}, when: at(i + 1)));
      case 5:
        flat.add(ev('q$i', 'approval.requested', actor: agentActor,
            data: {'call_id': cid}, when: at(i + 1)));
      case 6:
        flat.add(ev('x$i', 'approval.decided', actor: agentActor,
            data: {'call_id': cid, 'approve': rng.nextBool()}, when: at(i + 1)));
      case 7:
        flat.add(ev('n$i', 'tool_call.interrupted', actor: agentActor,
            data: {'call_id': cid}, when: at(i + 1)));
      case 8:
        flat.add(ev('s$i', 'turn.state', actor: agentActor,
            data: {'state': states[rng.nextInt(states.length)]},
            when: at(i + 1)));
    }
  }
  final chunks = <List<HfyEvent>>[];
  final actions = <String>[];
  var i = 0;
  while (i < flat.length) {
    final end = (i + 1 + rng.nextInt(3)).clamp(0, flat.length);
    chunks.add(flat.sublist(i, end));
    i = end;
    actions.add(const ['', 'min', 'open'][rng.nextInt(3)]);
  }
  return (chunks, actions);
}

/// The fold's public story to compare across the two paths: sheet outlets,
/// pending order and per-turn shape. [revision] deliberately stays out —
/// one batch bumps once while n applies bump n times, by contract.
Map<String, Object?> _viewStory(HfyConversation c) => <String, Object?>{
      'pendingOwn': c.pendingOwn,
      'sheetOpen': c.sheetOpen,
      'showActionPill': c.showActionPill,
      'ownActive': c.ownActive,
      'showWelcome': c.showWelcome,
      'turns': [
        for (final t in c.turns)
          <String, Object?>{
            'id': t.id,
            'state': t.state,
            'own': t.own,
            'showTyping': t.showTyping,
            'blocks': [
              for (final b in t.blocks)
                <String, Object?>{'id': b.id, 'streaming': b.streaming},
            ],
          },
      ],
    };

void main() {
  group('immutable fold views', () {
    HfyConversation folded() {
      final c = foldOf();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {'text': 'with picture'}, when: at(0)));
      c.apply(ev('b2', 'message.text', data: {
        'text': 'attachment moment',
        'attachments': [
          {'url': '/v1/files/a.png', 'mime': 'image/png', 'name': 'a.png'},
          {'url': '/v1/files/b.pdf', 'mime': 'application/pdf', 'name': 'b.pdf'},
        ],
      }, when: at(1)));
      c.apply(
        ev('tb', 'tool_call', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'}),
      );
      return c;
    }

    test('turns cannot gain/lose/replace', () {
      final c = folded();
      expect(
        () => c.turns.add(c.turns.first),
        throwsUnsupportedError,
      );
      expect(() => c.turns.removeAt(0), throwsUnsupportedError);
      expect(() => c.turns[0] = c.turns.first, throwsUnsupportedError);
      expect(() => c.turns.clear(), throwsUnsupportedError);
    });

    test('turn.blocks cannot gain/lose/replace', () {
      final c = folded();
      final blocks = c.t1.blocks;
      expect(() => blocks.add(blocks.first), throwsUnsupportedError);
      expect(() => blocks.removeAt(0), throwsUnsupportedError);
      expect(blocks.map((b) => b.id), containsAll(['b1', 'b2']));
    });

    test("blocks' images/files cannot gain/lose/replace", () {
      final c = folded();
      final text = c.t1.blocks.where((b) => b.isText).last;
      expect(text.images, hasLength(1));
      expect(text.files, hasLength(1));
      expect(() => text.images.add(text.images.first), throwsUnsupportedError);
      expect(() => text.files.removeAt(0), throwsUnsupportedError);
      expect(
        () => text.images[0] = text.images.first,
        throwsUnsupportedError,
      );
      // the fold keeps splitting into internal lists — new blocks still fold
      c.apply(ev('b3', 'message.text', data: {
        'text': 'more',
        'attachments': [
          {'url': 'https://embed.example/v1/files/c.png', 'mime': 'image/jpeg'},
        ],
      }, when: at(2)));
      expect(c.t1.blocks.where((b) => b.isText).last.images, hasLength(1));
    });

    test('captured reads are never live aliases of fold state', () {
      final c = folded();
      final turnsAtStart = c.turns;
      final blocksAtStart = c.t1.blocks;
      c.apply(
        ev('tr', 'tool_result', actor: agentActor, data: {'call_id': 'c1', 'content': 'ok'}),
      );
      expect(turnsAtStart.length, 1, reason: 'the captured list stayed frozen');
      expect(blocksAtStart.length, 3, reason: 'the captured blocks did not grow');
      // …while the live reads see everything
      expect(c.t1.blocks.last.content, 'ok');
    });

    test('per-block identity is stable across late folds', () {
      final c = folded();
      final before = c.t1.blocks[2]; // the tool row
      c.apply(
        ev('ar', 'approval.requested', actor: agentActor, data: {'call_id': 'c1'}),
      );
      c.apply(
        ev('ad', 'approval.decided', actor: agentActor,
            data: {'call_id': 'c1', 'approve': true}),
      );
      expect(identical(before, c.t1.blocks[2]), isTrue);
      expect(before.decision, 'allowed');
    });

    test('turns view: cached between state changes, fresh after each', () {
      final c = folded();
      final a = c.turns;
      final b = c.turns;
      expect(identical(a, b), isTrue, reason: 'no copy per read');
      c.apply(
        ev('ar', 'approval.requested', actor: agentActor, data: {'call_id': 'c1'}),
      );
      expect(identical(a, c.turns), isFalse, reason: 'revision moved, view rebuilt');
      expect(c.turns.length, a.length); // …but the CONTENT is the same set
    });
  });

  group('controller API: apply booleans, revision, encapsulated hasOlder/fileLabel', () {
    test('apply() is true when the fold acted, false only for dropped types', () {
      final c = foldOf();
      c.markLoaded();
      var revision = c.revision;
      expect(revision, 0, reason: 'a fresh conversation starts at zero');
      expect(c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'})), isTrue);
      expect(c.revision, revision + 1);
      revision = c.revision;
      expect(
        c.apply(ev('s1', 'turn.state', actor: agentActor, data: {'state': 'running'})),
        isTrue,
      );
      expect(c.revision, revision + 1);
      revision = c.revision;
      // text.delta pure-append: still a state change (text grew)
      expect(
        c.apply(ev('d1', 'text.delta', actor: agentActor, data: {'text': 'he'})),
        isTrue,
      );
      expect(c.revision, revision + 1);
      revision = c.revision;
      expect(
        c.apply(ev('u1', 'summary', actor: agentActor, data: {'text': 'x'})),
        isFalse,
        reason: 'dropped type — no relayout, no revision (ui.js default-return)',
      );
      expect(c.revision, revision);
    });

    test('a ghost decision still seats the turn, exactly like ui.js:1044-1046',
        () {
      // THE review repro: a first-ever approval.decided whose call_id the
      // fold never saw. ui.js's _ensureTurn creates the turn and STILL runs
      // relayout + syncDerived after the silent find; the old fold skipped
      // all of it and left the turns view stale over an internal turn.
      final c = foldOf();
      final revision = c.revision;
      expect(
        c.apply(
          ev('zz', 'approval.decided', actor: agentActor, data: {'call_id': 'ghost'}),
        ),
        isTrue,
        reason: 'handled: the turn was ensured even though nothing settled',
      );
      expect(c.revision, revision + 1, reason: 'the view must move with it');
      expect(c.turns, hasLength(1), reason: 'the turns VIEW seats the turn');
      expect(c.turns.single.id, 'turn-1');
      expect(c.turns.single.state, 'queued');
      expect(c.turns.single.own, isFalse);
      expect(c.turns.single.showTyping, isTrue,
          reason: 'ui.js relayout: an empty queued turn shows the dots');
      expect(c.showWelcome, isFalse, reason: 'turns exist — the empty state is gone');
      expect(c.pendingOwn, isEmpty);
      expect(c.sheetOpen, isFalse);
      expect(c.ownActive, isFalse);
      // an unknown-call interrupted behaves the same
      final c2 = foldOf();
      final r2 = c2.revision;
      expect(
        c2.apply(
          ev('ii', 'tool_call.interrupted', actor: agentActor, data: {'call_id': 'g2'}),
        ),
        isTrue,
      );
      expect(c2.revision, r2 + 1);
      expect(c2.turns.single.state, 'queued');
      expect(c2.turns.single.showTyping, isTrue);
    });

    test('applyAll bumps at most once and reports whether anything changed', () {
      final c = foldOf();
      c.markLoaded();
      expect(c.revision, 0);
      expect(
        c.applyAll([
          ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}),
          ev('b1', 'message.text', data: {'text': 'a'}),
          ev('b2', 'message.text', data: {'text': 'b'}),
          ev('u1', 'summary', actor: agentActor, data: {'text': 'noise'}),
        ]),
        isTrue,
      );
      expect(c.revision, 1, reason: 'one batch — one bump');
      expect(c.applyAll([ev('u2', 'unicorns', data: const {})]), isFalse);
      expect(c.revision, 1, reason: 'unknown types are not handled — no bump');
    });

    test('a ghost-only batch seats its turn with the same one-bump discipline',
        () {
      final c = foldOf();
      final revision = c.revision;
      // returns whether anything SETTLED (false) — but the batch was handled,
      // so the turn seats in the view with its relayout-driven typing dots.
      expect(
        c.applyAll([
          ev('zz', 'approval.decided', actor: agentActor, data: {'call_id': 'ghost'}),
        ]),
        isFalse,
      );
      expect(c.revision, revision + 1, reason: 'the fold acted (turn created)');
      expect(c.turns.single.state, 'queued');
      expect(c.turns.single.showTyping, isTrue);
    });

    test('hasOlder is a getter — setHasOlder mirrors connect and re-derives', () {
      final c = foldOf();
      c.markLoaded();
      c.syncDerived();
      expect(c.showWelcome, isTrue);
      expect(c.hasOlder, isFalse);
      c.setHasOlder(true);
      expect(c.hasOlder, isTrue);
      expect(c.showWelcome, isFalse, reason: 'syncDerived ran (ui.js:861)');
      // and mergeOlderPage keeps the loadOlder path setting it (ui.js:1241)
      c.mergeOlderPage([
        ev('turn-0', 'turn', parent: '', data: {'agent': 'main'}),
        ev('m0', 'message.text', parent: 'turn-0', data: {'text': 'older'}),
        ev('s0', 'turn.state', parent: 'turn-0', actor: agentActor,
            data: {'state': 'completed'}),
      ], hasMore: true);
      expect(c.hasOlder, isTrue);
      c.mergeOlderPage(const <HfyEvent>[], hasMore: false);
      expect(c.hasOlder, isFalse);
    });

    test('fileLabel comes in at construction and feeds the labels', () {
      final c = foldOf();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {
        'text': 'doc',
        'attachments': [
          {'url': 'https://embed.example/v1/files/x'},
        ],
      }));
      expect(
        c.t1.blocks.where((b) => b.isText).last.files.single.label,
        'File',
        reason: "the en strings' t('file') fallback",
      );
      final german = HfyConversation(
        viewer: const HfyViewer(uuid: u1, ref: u1),
        base: embedExample,
        tz: const HfyTz.utc(),
        fileLabel: 'Datei',
      );
      german.markLoaded();
      german.apply(ev('b1', 'message.text', data: {
        'text': 'doc',
        'attachments': [
          {'url': 'https://embed.example/v1/files/x'},
        ],
      }));
      expect(
        german.turns.first.blocks.last.files.single.label,
        'Datei',
      );
    });
  });

  group('batch folding with minimize interleave (P-FOLD-049 order rules)', () {
    // Sheet bookkeeping (_sheetMin/_seenPending) is the ONE order-sensitive
    // sheet state, so applyAll folds it per event via _sheetFoldCheck while
    // deferring the expensive relayout. These tests pin event-at-a-time
    // equivalence WITH minimize/open points; revision is deliberately not
    // compared (one batch bumps once, n applies bump n times — by contract).
    HfyConversation prepped() {
      final c = foldOf();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {'text': 'pay please'}));
      c.apply(
        ev('ta', 'tool_call', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'}),
      );
      c.apply(
        ev('ra', 'approval.requested', actor: agentActor, data: {'call_id': 'c1'}),
      );
      c.minimizeSheet();
      c.relayout();
      c.syncDerived();
      expect(c.pendingOwn, ['c1'],
          reason: 'prestate: c1 pends and the sheet sits minimized');
      expect(c.sheetOpen, isFalse);
      expect(c.showActionPill, isTrue);
      return c;
    }

    final batch = [
      ev('tb', 'tool_call', actor: agentActor,
          data: {'name': 'shop.pay', 'call_id': 'c2'}),
      ev('rb', 'approval.requested', actor: agentActor, data: {'call_id': 'c2'}),
      ev('db', 'approval.decided', actor: agentActor,
          data: {'call_id': 'c2', 'approve': false}),
    ];

    test('a fresh request inside the batch reopens the minimized sheet', () {
      // the exact review repro: per-event folding surfaces the fresh request
      // (reopening the sheet), decides it, and ends with c1 pending, OPEN.
      // Seeing only the end state would leave the sheet minimized.
      final batched = prepped();
      final perEvent = prepped();
      batched.applyAll(batch);
      for (final e in batch) {
        perEvent.apply(e);
      }
      for (final (label, c) in [('batch', batched), ('per-event', perEvent)]) {
        expect(c.pendingOwn, ['c1'], reason: '$label: c2 was decided in-batch');
        expect(c.sheetOpen, isTrue, reason: '$label: the fresh request opened');
        expect(c.showActionPill, isFalse, reason: '$label: not minimized');
      }
    });

    test('a re-request after a batched decided is NOT fresh (seen-ness '
        'survives the batch)', () {
      // the fate that pins the per-fold bookkeeping: batch1 requests and
      // decides c2 on turn-1; a later batch re-requests the same call id on
      // turn-2. Event-at-a-time, c2 entered _seenPending at its request —
      // the sheet must stay minimized (just the pill). A fold that only saw
      // the end state would never have marked c2 and would wrongly reopen.
      final batched = prepped();
      final perEvent = prepped();
      batched.applyAll(batch);
      for (final e in batch) {
        perEvent.apply(e);
      }
      final second = [
        ev('tu', 'turn', parent: '', data: {'agent': 'main'}),
        ev('bu', 'message.text', parent: 'turn-2', data: {'text': 'again'}),
        ev('tc2', 'tool_call', parent: 'turn-2', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c2'}),
        ev('rq2', 'approval.requested', parent: 'turn-2', actor: agentActor,
            data: {'call_id': 'c2'}),
      ];
      batched.minimizeSheet(); // the user re-faced the pill in between
      batched.relayout();
      batched.syncDerived();
      perEvent.minimizeSheet();
      perEvent.relayout();
      perEvent.syncDerived();
      batched.applyAll(second);
      for (final e in second) {
        perEvent.apply(e);
      }
      for (final (label, c) in [('batch', batched), ('per-event', perEvent)]) {
        expect(c.pendingOwn, ['c1', 'c2'], reason: '$label: c2 pends again');
        expect(c.sheetOpen, isFalse, reason: '$label: c2 is long seen');
        expect(c.showActionPill, isTrue, reason: label);
      }
    });

    test('randomized chunks with the SAME minimize/open points land identical',
        () {
      final rng = Random(20261031); // deterministic: failures replay exactly
      for (var trial = 0; trial < 40; trial++) {
        final (chunks, actions) = _randomChunks(rng);
        final batched = foldOf();
        final perEvent = foldOf();
        for (var i = 0; i < chunks.length; i++) {
          batched.applyAll(chunks[i]);
          for (final e in chunks[i]) {
            perEvent.apply(e);
          }
          if (actions[i] == 'min') {
            batched.minimizeSheet();
            perEvent.minimizeSheet();
          } else if (actions[i] == 'open') {
            batched.openSheet();
            perEvent.openSheet();
          }
          for (final c in [batched, perEvent]) {
            c.relayout();
            c.syncDerived();
          }
        }
        batched.relayout();
        batched.syncDerived();
        perEvent.relayout();
        perEvent.syncDerived();
        final diff = firstJsonDifference(_viewStory(batched), _viewStory(perEvent));
        expect(
          diff,
          isNull,
          reason: 'trial #$trial: the same chunk boundaries and sheet actions '
              'must not split the paths; first diff: $diff',
        );
      }
    });
  });

  group('batch folding: applyAll == per-event apply (fixtures + randomized orders)',
      () {
    test('every shared fixture projects identically through both paths', () {
      final fixtures = fixtureFiles();
      expect(fixtures, isNotEmpty);
      for (final def in fixtures) {
        // minimizeAfter is a USER action delivered at a position in the
        // event stream: the applyAll variant cannot interleave it (see
        // fixture_harness.dart), and a shuffled order also changes which
        // pending calls re-open the sheet afterwards. The canonical
        // projection of such scenarios is pinned by test/fixtures_test.dart.
        if (def['minimizeAfter'] != null) continue;
        final perEvent = runScenario(def);
        final batch = runScenario(def, batch: true);
        final diff = firstJsonDifference(batch, perEvent);
        expect(
          diff,
          isNull,
          reason: '${def['name']}: batched folds must land on the identical'
              ' projection (relayout is a full recompute); first diff: $diff',
        );
      }
    });

    test('randomized orderings project identically through both paths too', () {
      final rng = Random(2026); // deterministic: a failure reproduces exactly
      final fixtures = fixtureFiles();
      for (final fixture in fixtures) {
        // a shuffled minimizeAfter changes where the sheet is closed, see the
        // skip reason in the test above
        if (fixture['minimizeAfter'] != null) continue;
        for (var attempt = 0; attempt < 8; attempt++) {
          final def = jsonDecode(jsonEncode(fixture))
              as Map<String, dynamic>; // deep copy, then shuffle its events
          shuffleEvents(def, rng);
          final diff = firstJsonDifference(
            runScenario(def, batch: true),
            runScenario(def),
          );
          expect(
            diff,
            isNull,
            reason: '${def['name']} shuffle #$attempt must compare equal: '
                'first diff: $diff',
          );
        }
      }
    });

    test('the delta→terminal→durable pattern folds the same either way (the '
        'stream-relayout hazard the batch fold explicitly carries over)', () {
      final sequence = [
        ev('d1', 'text.delta', actor: agentActor, data: {'text': 'Hel'}),
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'cancelled'}),
        ev('m1', 'message.text', actor: agentActor, data: {'text': 'Hello, world!'}),
      ];

      HfyConversation run({required bool batch}) {
        final c = foldOf();
        c.markLoaded();
        c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
        if (batch) {
          c.applyAll(sequence);
        } else {
          for (final e in sequence) {
            c.apply(e);
          }
        }
        c.relayout();
        c.syncDerived();
        return c;
      }

      final perEvent = run(batch: false);
      final batched = run(batch: true);
      final ids = [for (final b in batched.t1.blocks) b.id];
      expect(ids, hasLength(2));
      expect(perEvent.t1.blocks[0].text, 'Hel');
      expect(batched.t1.blocks[0].text, 'Hel');
      expect(perEvent.t1.blocks[0].streaming, isFalse, reason: 'terminal turn');
      expect(batched.t1.blocks[0].streaming, isFalse, reason: 'same kill');
      expect(batched.t1.blocks.where((b) => b.id == 'm1'), hasLength(1));
      expect(
        identical(
          perEvent.t1.blocks.map((b) => b.id).toList(),
          batched.t1.blocks.map((b) => b.id).toList(),
        ),
        isFalse,
        reason: 'sanity: not comparing the same list object',
      );
      expect(
        [
          for (final b in perEvent.t1.blocks) b.id,
        ],
        ids,
        reason: 'both paths ended with the same blocks',
      );

      // without the inline terminal kill the batched fold would REPLACE the
      // dead stream block in place — assert the durable text made a FRESH row
      expect(
        batched.t1.blocks[0].id == 'turn-1:stream' &&
            batched.t1.blocks[1].id == 'm1',
        isTrue,
      );
    });
  });

  group('mergeOlderPage: ONE atomic call (replaces applyHistoryPage/mergeOlder)', () {
    test('happy path: fold without layout, merge, one relayout', () {
      final c = foldOf();
      c.markLoaded();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {'text': 'newer'}));
      final revision = c.revision;
      c.mergeOlderPage([
        ev('turn-0', 'turn', parent: '', data: {'agent': 'main'}),
        ev('m0', 'message.text', parent: 'turn-0', data: {'text': 'older'}),
        ev('s0', 'turn.state', parent: 'turn-0', actor: agentActor,
            data: {'state': 'running'}),
      ], hasMore: true);
      expect(c.hasOlder, isTrue);
      expect(c.revision, revision + 1);
      c.mergeOlderPage(const <HfyEvent>[], hasMore: false);
      expect(c.hasOlder, isFalse);
      expect(c.revision, revision + 2, reason: 'hasOlder moved again — a real '
          'state change (showWelcome watches it)');
    });

    test('failure path: a throwing page never strands the collector', () {
      final c = foldOf();
      c.markLoaded();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {'text': 'newer'}));
      final revision = c.revision;
      final hasOlder = c.hasOlder;
      final poison = HfyEvent(
        id: 'poison',
        type: 'message.text',
        parent: 'turn-0',
        actor: agentActor,
        data: {
          'text': 'x',
          'attachments': [
            <String, Object?>{'url': 'https://embed.example/v1/a', 'mime': _Shatter()},
          ],
        },
        createdAt: at(0),
      );
      expect(
        () => c.mergeOlderPage([poison], hasMore: true),
        throwsStateError,
      );
      expect(c.hasOlder, hasOlder, reason: 'the page was discarded whole');
      expect(c.revision, revision, reason: 'no merge ran — no bump');
      expect([for (final t in c.turns) t.id], ['turn-1'],
          reason: 'no partial turn leaked');
      // and the next (good) page works — the old two-step API would have
      // swallowed it into a stuck collector
      c.mergeOlderPage([
        ev('turn-0', 'turn', parent: '', data: {'agent': 'main'}),
        ev('m0', 'message.text', parent: 'turn-0', data: {'text': 'older'}),
        ev('old-stamp', 'message.text', parent: 'turn-1', data: {'text': 'stamp'}),
      ], hasMore: false);
      expect([for (final t in c.turns) t.id], ['turn-0', 'turn-1']);
      expect(c.t1.blocks.map((b) => b.id), contains('old-stamp'));
    });
  });

  group('mergeOlderPage folds like the ui.js collector (no per-event relayout)',
      () {
    // ui.js folds collection events and returns BEFORE relayouting them
    // (ui.js:1043), so a page's terminal state does not kill a ranked-in
    // stream block at fold time — a later page event's durable text still
    // finds it alive and replaces it in place, like ui.js's merged pages.
    test('delta → terminal → durable lands in ONE block, as ui.js folds pages',
        () {
      final c = foldOf();
      c.mergeOlderPage([
        ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}),
        ev('d1', 'text.delta', actor: agentActor, data: {'text': 'Hel'}),
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'cancelled'}),
        ev('m1', 'message.text', actor: agentActor,
            data: {'text': 'Hello, world!'}),
      ], hasMore: false);
      // the collector kept the stream block alive for the in-page durable
      // text (ui.js replacement, ui.js:975-979) instead of leaving a dead
      // ':stream' twin behind
      expect(c.t1.blocks.map((b) => b.id), ['m1']);
      expect(c.t1.blocks.single.text, 'Hello, world!');
      expect(c.t1.blocks.single.streaming, isFalse,
          reason: 'the merge relayout killed the streaming flag');
      expect(c.t1.state, 'cancelled');
    });

    test('a page delta after the terminal state is born alive and dies at the '
        'one relayout (the synthetic block stays, like ui.js)', () {
      final c = foldOf();
      c.mergeOlderPage([
        ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}),
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'cancelled'}),
        ev('d1', 'text.delta', actor: agentActor, data: {'text': 'Hel'}),
      ], hasMore: false);
      expect(c.t1.blocks.map((b) => b.id), ['turn-1:stream']);
      expect(c.t1.blocks.single.text, 'Hel');
      expect(c.t1.blocks.single.streaming, isFalse,
          reason: 'only the merge relayout settles it — no per-event kill');
    });

    test('the same collector rule covers reasoning deltas', () {
      final c = foldOf();
      c.mergeOlderPage([
        ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}),
        ev('r1', 'reasoning.delta', actor: agentActor, data: {'text': 'think'}),
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'completed'}),
        ev('rr', 'reasoning', actor: agentActor,
            data: {'text': 'the full thought'}),
      ], hasMore: false);
      expect(c.t1.blocks.map((b) => b.id), ['rr']);
      expect(c.t1.blocks.single.text, 'the full thought');
      expect(c.t1.blocks.single.streaming, isFalse);
    });
  });

  group('markSubmitted: a sheet POST leaves pendingOwn before the decision '
      '(ui.js:1110, P-FOLD-042)', () {
    HfyConversation twoPending() {
      final c = foldOf();
      c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(ev('b1', 'message.text', data: {'text': 'pay both'}));
      for (final cid in ['c1', 'c2']) {
        c.apply(ev('t$cid', 'tool_call', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': cid}));
        c.apply(ev('r$cid', 'approval.requested', actor: agentActor,
            data: {'call_id': cid}));
      }
      expect(c.pendingOwn, ['c1', 'c2'], reason: 'prestate');
      expect(c.sheetOpen, isTrue, reason: 'prestate');
      return c;
    }

    test('drops the call from pendingOwn and the sheet, one revision per call',
        () {
      final c = twoPending();
      final before = c.revision;
      c.markSubmitted('c1');
      expect(c.pendingOwn, ['c2']);
      expect([for (final b in c.pendingOwnBlocks) b.callId], ['c2']);
      expect(c.revision, before + 1);
      c.markSubmitted('c2');
      expect(c.pendingOwn, isEmpty);
      expect(c.sheetOpen, isFalse);
      expect(c.revision, before + 2);
    });

    test('is idempotent and ignores unknown call ids', () {
      final c = twoPending();
      c.markSubmitted('c1');
      final after = c.revision;
      c.markSubmitted('c1');
      c.markSubmitted('ghost');
      expect(c.revision, after, reason: 'nothing changed, nothing bumped');
      expect(c.pendingOwn, ['c2']);
    });

    test('stays out through later folds and settles on the decision', () {
      final c = twoPending();
      c.markSubmitted('c1');
      c.apply(ev('d1', 'text.delta', actor: agentActor, data: {'text': 'ok'}));
      expect(c.pendingOwn, ['c2'], reason: 'a relayout does not resurrect it');
      c.apply(ev('x1', 'approval.decided', actor: agentActor,
          data: {'call_id': 'c1', 'approve': true}));
      expect(c.pendingOwn, ['c2']);
      final block = c.t1.blocks.firstWhere((b) => b.callId == 'c1');
      expect(block.showDecided, isTrue);
      expect(block.decision, 'allowed');
    });

    test('a minimized sheet stays minimized while the marked call leaves', () {
      final c = twoPending();
      c.minimizeSheet();
      c.markSubmitted('c1');
      expect(c.sheetOpen, isFalse);
      expect(c.showActionPill, isTrue, reason: 'c2 still pends');
      c.markSubmitted('c2');
      expect(c.showActionPill, isFalse);
    });

    test('pendingOwnBlocks keeps its instance until the fold changes and '
        'cannot be mutated', () {
      final c = twoPending();
      final list = c.pendingOwnBlocks;
      c.apply(ev('k', 'model.usage', data: {'tokens': 1}));
      expect(identical(c.pendingOwnBlocks, list), isTrue,
          reason: 'an unhandled event relayouts nothing');
      expect(() => c.pendingOwnBlocks.clear(), throwsUnsupportedError);
    });

    test('a relayout that moves no pending row keeps the same sheet list', () {
      final c = twoPending();
      final list = c.pendingOwnBlocks;
      final rev = c.revision;
      c.mergeOlderPage(const <HfyEvent>[], hasMore: false);
      expect(c.revision, rev, reason: 'an empty merge with hasMore steady');
      expect(identical(c.pendingOwnBlocks, list), isTrue,
          reason: 'the rows are the same, so the list is too');
      c.markSubmitted('c1');
      expect(identical(c.pendingOwnBlocks, list), isFalse,
          reason: 'a row left: the list is rebuilt');
      expect([for (final b in c.pendingOwnBlocks) b.callId], ['c2']);
    });
  });

  group('smoke: 10k-event history folds in a batch (NOT a benchmark)', () {
    test('applyAll under the generous bound with sanity checks', () {
      final events = tenKEvents();
      final c = foldOf();
      c.markLoaded();
      // warmup (~50 events) so JIT noise cannot flake the bound; this is a
      // smoke guard that the fold stays O(n) per page, never a benchmark.
      c.applyAll(events.take(51), history: true);
      c.applyAll([], history: true);
      final c2 = foldOf();
      c2.markLoaded();
      final sw = Stopwatch()..start();
      c2.applyAll(events);
      sw.stop();
      expect(c2.turns, hasLength(1));
      expect(c2.t1.blocks, hasLength(6000), reason: '2000 rounds of (user text, '
          'delta-then-durable-replace → 1 agent text, tool row) = 3 blocks '
          'each; the delta folds INTO the reply until its message.text lands');
      // The generous desktop bound: 1.5 s. Phones are 3-5× slower, but the
      // page fold must stay linear: a relayout-per-event history load would
      // not just slide past this, it would wreck a phone long before.
      expect(
        sw.elapsedMilliseconds,
        lessThan(1500),
        reason: 'smoke guard, not a benchmark: 10k events at ${sw.elapsedMilliseconds} ms',
      );
    });
  });
}
