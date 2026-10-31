// Fold PARITY rules pinned as unit tests with their PARITY IDs in group
// names: P-FOLD-021 own-turn detection (first-starter rule), P-FOLD-022 the
// awaitTurn bridge, P-FOLD-024 the full tool-visibility matrix, P-FOLD-057
// unknown events, ui.js findTool pairing edges, ask option labels (label !=
// null parity) and the time-zone semantics of P-FOLD-027 including the
// local()-default proof.

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/model/ask.dart';
import 'package:hfy_flutter/src/model/event.dart';
import 'package:hfy_flutter/src/model/fold.dart';
import 'package:hfy_flutter/src/model/state.dart';

import '../support/fixture_harness.dart' show embedExample;

// 2026-01-02 10:00-based UTC stamps, mirroring fold_rules_test.dart.
DateTime at(int min, [int s = 0]) => DateTime.utc(2026, 1, 2, 10, min, s);
const u1 = 'u1';
final userActor = HfyActor(uuid: u1, type: 'external', ref: u1);
final user2Actor = HfyActor(uuid: 'u2', type: 'external', ref: 'u2');
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

HfyConversation foldOf({
  HfyViewer? viewer,
  HfyToolCalls mode = HfyToolCalls.summary,
  HfyTz? tz,
}) {
  final c = HfyConversation(
    viewer: viewer ?? const HfyViewer(uuid: u1, type: 'external', ref: u1),
    toolCalls: mode,
    base: embedExample,
    tz: tz ?? const HfyTz.utc(),
  );
  c.markLoaded();
  return c;
}

void startTurn(HfyConversation c) {
  c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
}

extension _Find on HfyConversation {
  HfyTurn get t1 => turns.firstWhere((t) => t.id == 'turn-1');
  HfyBlock byId(String id) => t1.blocks.firstWhere((b) => b.id == id);
  HfyBlock toolRow(String callId) =>
      t1.blocks.firstWhere((b) => b.isTool && b.callId == callId);
}

/// The visibility matrix walks one call through a phase: running, failed
/// result, decided approval, open approval.
void toolPhase(HfyConversation c, String phase) {
  switch (phase) {
    case 'running':
      c.apply(
        ev('tb', 'tool_call', actor: agentActor, data: {
          'name': 'files.read', 'call_id': 'c1', 'arguments': {'n': 1},
        }),
      );
    case 'error':
      toolPhase(c, 'running');
      c.apply(
        ev('tr', 'tool_result', actor: agentActor,
            data: {'call_id': 'c1', 'content': 'boom', 'is_error': true}),
      );
    case 'decided':
      toolPhase(c, 'running');
      c.apply(
        ev('ar', 'approval.requested', actor: agentActor, data: {'call_id': 'c1'}),
      );
      c.apply(
        ev('ad', 'approval.decided', actor: agentActor,
            data: {'call_id': 'c1', 'approve': true}),
      );
    case 'pending':
      toolPhase(c, 'running');
      c.apply(
        ev('ar', 'approval.requested', actor: agentActor, data: {'call_id': 'c1'}),
      );
    default:
      throw ArgumentError(phase);
  }
}

void main() {
  group('P-FOLD-021 own turn = the FIRST user text (first-starter rule)', () {
    test("another user's text first keeps the turn foreign even when the "
        'viewer speaks later', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('m1', 'message.text', actor: user2Actor, data: {'text': 'hi all'}));
      c.apply(ev('m2', 'message.text', data: {'text': 'my question'}));
      final turn = c.t1;
      expect(turn.own, isFalse, reason: 'the FIRST user text started the turn');
      expect(turn.userLabel, 'u2');
      expect(c.ownActive, isFalse);
    });

    test('mirror case: the viewer first, another user later — own stays true', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('m1', 'message.text', data: {'text': 'my question'}));
      c.apply(ev('m2', 'message.text', actor: user2Actor, data: {'text': 'hi all'}));
      expect(c.t1.own, isTrue);
    });

    test('a turn with no user text is never own, whatever else arrives', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('m1', 'message.text', actor: agentActor, data: {'text': 'greeting'}));
      c.apply(
        ev('tb', 'tool_call', actor: agentActor, data: {'name': 'x', 'call_id': 'c1'}),
      );
      expect(c.t1.own, isFalse);
      expect(c.t1.userLabel, '');
    });

    test('per-block own flags follow each block; the turn flag does not flip', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('m1', 'message.text', actor: user2Actor, data: {'text': 'their words'}));
      c.apply(ev('m2', 'message.text', data: {'text': 'my words'}));
      expect(c.byId('m1').own, isFalse);
      expect(c.byId('m2').own, isTrue);
      expect(c.t1.own, isFalse); // still the first-starter's
    });
  });

  group("P-FOLD-022 awaitTurn bridges send() and the turn's first user text", () {
    test('the bridge alone switches stop-mode on and off', () {
      final c = foldOf();
      expect(c.ownActive, isFalse);
      c.awaitTurn('turn-fresh');
      expect(c.ownActive, isTrue);
      c.clearAwaitTurn();
      expect(c.ownActive, isFalse);
    });

    test("the bridge clears when ITS turn's text lands, and only then", () {
      final c = foldOf();
      // turn-5 and turn-7 are already terminal: nothing of the viewer's is
      // active, so ownActive reads the bridge alone.
      for (final tid in ['turn-5', 'turn-7']) {
        c.apply(ev(tid, 'turn', parent: '', data: {'agent': 'main'}));
        c.apply(
          ev('s-$tid', 'turn.state', parent: tid, actor: agentActor,
              data: {'state': 'completed'}),
        );
      }
      c.awaitTurn('turn-7');
      c.apply(ev('m5', 'message.text', parent: 'turn-5', data: {'text': 'nope'}));
      expect(
        c.ownActive,
        isTrue,
        reason: "turn-5's text landing does not satisfy turn-7's bridge",
      );
      c.apply(ev('m7', 'message.text', parent: 'turn-7', data: {'text': 'yes'}));
      expect(c.ownActive, isFalse, reason: 'the bridge landed and cleared');
    });

    test('the bridge keeps waiting while no user text lands anywhere', () {
      final c = foldOf();
      c.apply(
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'running'}),
      );
      expect(c.ownActive, isFalse, reason: "not the viewer's turn");
      c.awaitTurn('turn-1');
      expect(c.ownActive, isTrue);
      c.apply(ev('m1', 'message.text', actor: agentActor, data: {'text': 'agent text'}));
      expect(
        c.ownActive,
        isTrue,
        reason: 'only a USER text bridges the gap (ui.js:799-800)',
      );
    });
  });

  group('P-FOLD-024 tool-visibility matrix (mode × running|error|decided|pending)', () {
    // ui.js:1126-1131's matrix. Reference: every surfaced tool row is a
    // summary row (under `summary`), decided approvals ALSO show under
    // `hidden`, `details` shows details everywhere; an OPEN approval never
    // hides and routes to the sheet via showPendingOwn/sheetOpen.
    Iterable<({String mode, String phase, HfyBlock tool, HfyConversation c})>
        matrix() sync* {
      for (final entry in {
        'hidden': HfyToolCalls.hidden,
        'summary': HfyToolCalls.summary,
        'details': HfyToolCalls.details,
      }.entries) {
        for (final phase in ['running', 'error', 'decided', 'pending']) {
          final c = foldOf(mode: entry.value);
          startTurn(c);
          c.apply(ev('b1', 'message.text', data: {'text': 'go'}));
          toolPhase(c, phase);
          yield (
            mode: entry.key,
            phase: phase,
            tool: c.t1.blocks.lastWhere((b) => b.isTool),
            c: c,
          );
        }
      }
    }

    test('statuses settle as the events set them', () {
      for (final row in matrix()) {
        expect(
          row.tool.status,
          row.phase == 'error' ? 'error' : 'running',
          reason: '${row.phase}/${row.mode}: decided/pending keep the call '
              'running until its tool_result folds',
        );
      }
    });

    test('open approvals: never hidden, never summarized, sheet owns them', () {
      for (final row in matrix().where((r) => r.phase == 'pending')) {
        expect(row.tool.showApproval, isTrue, reason: row.mode);
        expect(row.tool.showSummary, isFalse, reason: row.mode);
        expect(row.tool.showDetails, isFalse, reason: row.mode);
        expect(row.tool.hidden, isFalse, reason: row.mode);
        expect(row.tool.showPendingOwn, isTrue, reason: row.mode);
        expect(row.tool.showPendingOther, isFalse, reason: row.mode);
        expect(row.c.pendingOwn, ['c1'], reason: row.mode);
        expect(row.c.sheetOpen, isTrue, reason: row.mode);
      }
    });

    test('summary rows under summary mode, decided kept under hidden mode', () {
      for (final row in matrix()) {
        if (row.phase == 'pending') continue; // covered above
        final summaryRow =
            row.mode == 'summary' || (row.mode == 'hidden' && row.phase == 'decided');
        expect(
          row.tool.showSummary,
          summaryRow,
          reason: '${row.phase}/${row.mode}',
        );
        expect(
          row.tool.showDetails,
          row.mode == 'details',
          reason: '${row.phase}/${row.mode}',
        );
        expect(
          row.tool.hidden,
          row.mode == 'hidden' && row.phase != 'decided',
          reason: '${row.phase}/${row.mode}',
        );
      }
    });
  });

  group('P-FOLD-057 unknown event types are inert (no block, no layout)', () {
    test('apply() returns false and leaves revision and turns untouched', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('b1', 'message.text', data: {'text': 'hi'}));
      final revision = c.revision;
      final turnIds = [for (final t in c.turns) t.id];
      final blockIds = [for (final b in c.t1.blocks) b.id];
      for (final type in ['summary', 'model.usage', 'unicorns']) {
        expect(
          c.apply(ev('u-$type', type, data: {'text': 'noise'})),
          isFalse,
          reason: 'unknown/unused types drop (ui.js default-return)',
        );
      }
      expect(c.revision, revision, reason: 'no state change — no revision bump');
      expect([for (final t in c.turns) t.id], turnIds);
      expect([for (final b in c.t1.blocks) b.id], blockIds);
    });
  });

  group('tool pairing edges (ui.js findTool parity)', () {
    test('tool_result BEFORE tool_call creates an orphan; the call adds a second '
        'row; lookups stick to the FIRST', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev('tr', 'tool_result', actor: agentActor,
            data: {'call_id': 'cx', 'content': 'early'}),
      );
      c.apply(
        ev('tc', 'tool_call', actor: agentActor,
            data: {'name': 'files.read', 'call_id': 'cx'}),
      );
      final two = c.t1.blocks.where((b) => b.isTool).toList();
      expect(two, hasLength(2));
      expect(two[0].name, '', reason: 'orphan row (ui.js:1006)');
      expect(two[1].name, 'files.read');
      c.apply(
        ev('tr2', 'tool_result', actor: agentActor,
            data: {'call_id': 'cx', 'content': 'late'}),
      );
      final rows = c.t1.blocks.where((b) => b.isTool).toList();
      expect(rows[0].content, 'late', reason: '_findTool returns the first row');
      expect(rows[1].content, '');
      c.apply(
        ev('ti', 'tool_call.interrupted', actor: agentActor, data: {'call_id': 'cx'}),
      );
      expect(
        c.t1.blocks.where((b) => b.isTool).toList()[0].status,
        'interrupted',
      );
    });

    test('duplicate call_ids: results and decisions attach to the FIRST row', () {
      final c = foldOf();
      startTurn(c);
      for (final tid in ['a1', 'a2']) {
        c.apply(
          ev(tid, 'tool_call', actor: agentActor,
              data: {'name': 'files.read', 'call_id': 'dup'}),
        );
      }
      c.apply(
        ev('r1', 'tool_result', actor: agentActor,
            data: {'call_id': 'dup', 'content': 'ok'}),
      );
      var rows = c.t1.blocks.where((b) => b.isTool).toList();
      expect(rows[0].status, 'done');
      expect(rows[1].status, 'running', reason: 'only the first row pairs up');
      c.apply(
        ev('d1', 'approval.decided', actor: agentActor,
            data: {'call_id': 'dup', 'approve': false}),
      );
      rows = c.t1.blocks.where((b) => b.isTool).toList();
      expect(rows[0].decision, 'denied');
      expect(rows[1].decision, '');
    });

    test('approval.decided with no request: settles the row it knows, nothing '
        'for an unknown call', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev('tc', 'tool_call', actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'}),
      );
      // no approval.requested ever folds
      c.apply(
        ev('ad', 'approval.decided', actor: agentActor,
            data: {'call_id': 'c1', 'approve': true}),
      );
      expect(c.toolRow('c1').decision, 'allowed');
      expect(c.toolRow('c1').showDecided, isTrue);
      expect(c.toolRow('c1').showApproval, isFalse);
      final before = c.t1.blocks.length;
      expect(
        c.apply(
          ev('ad2', 'approval.decided', actor: agentActor, data: {'call_id': 'zz'}),
        ),
        isFalse,
        reason: 'no row to act on — ui.js mines it silently',
      );
      expect(c.t1.blocks.length, before);
    });

    test("a missing/null call_id NEVER matches an orphan row — every miss gets "
        "its own row; a present-but-empty id ('') DOES match one", () {
      final c = foldOf();
      startTurn(c);
      // r1: call_id absent → orphan row with callId ''
      c.apply(
        ev('r1', 'tool_result', actor: agentActor, data: {'content': 'one'}),
      );
      expect(c.t1.blocks.where((b) => b.isTool), hasLength(1));
      // r2: call_id present but '' → matches the orphan row ('' === '')
      c.apply(
        ev('r2', 'tool_result', actor: agentActor,
            data: {'call_id': '', 'content': 'two'}),
      );
      expect(c.t1.blocks.where((b) => b.isTool), hasLength(1), reason: 'matched');
      expect(c.toolRow('').content, 'two');
      // r3: absent again — `undefined === ''` is false → ANOTHER fresh row
      c.apply(
        ev('r3', 'tool_result', actor: agentActor, data: {'content': 'three'}),
      );
      final rows = c.t1.blocks.where((b) => b.isTool).toList();
      expect(rows, hasLength(2), reason: 'ui.js makes a fresh row per miss');
      expect(rows[0].content, 'two');
      expect(rows[1].content, 'three');
      // a decided for a MISSING call id does not even create a row
      final before = c.t1.blocks.length;
      expect(
        c.apply(ev('ad', 'approval.decided', actor: agentActor, data: {'approve': true})),
        isFalse,
      );
      expect(c.t1.blocks.length, before);
    });
  });

  group('ask option labels (ui.js `label != null ? label : value`)', () {
    List<HfyAskOption> options(Object raw) =>
        hfyAskQuestions(raw).expand((q) => q.options).toList();

    test("present falsy labels stay verbatim ('' / 0 / false); null falls back", () {
      final opts = options({
        'questions': [
          {
            'id': 'q',
            'type': 'single',
            'options': [
              {'value': 'a', 'label': ''},
              {'value': 'b', 'label': 0},
              {'value': 'c', 'label': false},
              {'value': 'd', 'label': null},
              {'value': 'e'}, // label absent → the value
            ],
          },
        ],
      });
      expect(opts[0].label, '', reason: "an empty label is a LABEL ('' is not fall-through)");
      expect(opts[1].label, '0');
      expect(opts[2].label, 'false');
      expect(opts[3].label, 'd');
      expect(opts[4].label, 'e');
      expect(opts.map((o) => o.value), ['a', 'b', 'c', 'd', 'e']);
    });
  });

  group('time zone (P-FOLD-027): fixed zones across midnight; local() is the default', () {
    test('day keys follow the injected zone across calendar borders', () {
      final cases = <(HfyTz, DateTime, String)>[
        // UTC-12: an instant already in Jan-3 UTC is Jan-2 locally
        (
          const HfyTz.fixed('Etc/GMT+12', Duration(hours: -12)),
          DateTime.utc(2026, 1, 3, 0, 30),
          '2026-01-02',
        ),
        // UTC+14 (Kiritimati): crosses its calendar border 14h before UTC
        (
          const HfyTz.fixed('Kiritimati', Duration(hours: 14)),
          DateTime.utc(2026, 1, 2, 10, 1),
          '2026-01-03',
        ),
        (
          const HfyTz.fixed('Kiritimati', Duration(hours: 14)),
          DateTime.utc(2026, 1, 2, 9, 59),
          '2026-01-02',
        ),
        // A half-hour zone places its midnight at 18:30Z
        (
          const HfyTz.fixed('Kolkata', Duration(hours: 5, minutes: 30)),
          DateTime.utc(2026, 1, 2, 18, 29, 59),
          '2026-01-02',
        ),
        (
          const HfyTz.fixed('Kolkata', Duration(hours: 5, minutes: 30)),
          DateTime.utc(2026, 1, 2, 18, 30, 1),
          '2026-01-03',
        ),
      ];
      for (final (tz, instant, key) in cases) {
        expect(tz.dayKey(instant.millisecondsSinceEpoch), key, reason: '$tz @ $instant');
      }
    });

    test('a showDay separator opens exactly at the ZONE calendar border', () {
      final tz = const HfyTz.fixed('Kiritimati', Duration(hours: 14));
      final c = foldOf(tz: tz);
      startTurn(c);
      // 09:59Z = zone Jan-2 23:59; 10:01Z = zone Jan-3 00:01
      c.apply(
        ev('b0', 'message.text', actor: agentActor, data: {'text': 'a'},
            when: DateTime.utc(2026, 1, 2, 9, 59)),
      );
      c.apply(
        ev('b1', 'message.text', actor: agentActor, data: {'text': 'b'},
            when: DateTime.utc(2026, 1, 2, 10, 1)),
      );
      expect(c.byId('b0').dayKey, '2026-01-02');
      expect(c.byId('b0').showDay, isTrue);
      expect(c.byId('b1').dayKey, '2026-01-03');
      expect(c.byId('b1').showDay, isTrue);
      expect(c.byId('b1').showHead, isTrue, reason: 'a day change is a head too');
    });

    test('HfyTz.local() is the DEFAULT zone: day keys read this machine\'s '
        'wall clock (DateTime.local), never a pinned UTC', () {
      // Build the fold WITHOUT a tz argument (the production default path).
      final conv = HfyConversation(
        viewer: const HfyViewer(uuid: u1, ref: u1),
        base: embedExample,
      );
      conv.markLoaded();
      conv.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
      conv.apply(ev('m1', 'message.text', data: {'text': 'when?'}, when: at(0)));
      final computed = conv.t1.blocks.first.dayKey;
      // What the test zone's own DateTime says this instant's calendar day is.
      final local = DateTime.fromMillisecondsSinceEpoch(at(0).millisecondsSinceEpoch);
      final expected = '${local.year.toString().padLeft(4, '0')}-'
          '${local.month.toString().padLeft(2, '0')}-'
          '${local.day.toString().padLeft(2, '0')}';
      expect(computed, expected);
      expect(const HfyTz.local().dayKey(at(0).millisecondsSinceEpoch), expected);
    });

    test('on a non-UTC machine the default zone visibly differs from pinned UTC',
        () {
      // 01:00Z lives in the PREVIOUS calendar day for UTC-1…-12 machines and
      // coincides on UTC machines; 12:00Z + 13h zones behave mirrored. Probe
      // both so any real offset (≥1h from UTC) shows at least one difference.
      final probes = [
        DateTime.utc(2026, 1, 2, 1, 0).millisecondsSinceEpoch,
        DateTime.utc(2026, 1, 2, 22, 0).millisecondsSinceEpoch,
        DateTime.utc(2026, 1, 2, 12, 0).millisecondsSinceEpoch,
      ];
      final utcKeys = [for (final p in probes) const HfyTz.utc().dayKey(p)];
      final localKeys = [for (final p in probes) const HfyTz.local().dayKey(p)];
      final hereIsUtc =
          DateTime.fromMillisecondsSinceEpoch(probes.first).timeZoneOffset ==
              Duration.zero;
      if (hereIsUtc) {
        expect(localKeys, utcKeys, reason: 'the machine itself runs UTC');
      } else {
        expect(
          localKeys,
          isNot(utcKeys),
          reason: 'the device zone is real (DateTime.local behaviour), never '
              'a silently pinned UTC',
        );
      }
    });

    test('an id-less durable reasoning keeps the prior id — documented divergence '
        'from ui.js:997, whose `b.id = env.id` would set undefined', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev('r1', 'reasoning.delta', actor: agentActor, data: {'text': 'think'},
            when: at(0, 1)),
      );
      c.apply(
        HfyEvent(
          type: 'reasoning', // NO id: ui.js:997 assigns b.id = env.id → undefined
          parent: 'turn-1',
          actor: agentActor,
          data: {'text': 'the full thought'},
          createdAt: at(0, 5),
        ),
      );
      final b = c.t1.blocks.single;
      expect(b.text, 'the full thought');
      expect(b.streaming, isFalse);
      expect(b.iso, '2026-01-02T10:00:01.000Z', reason: 'P-FOLD-058: NOT restamped');
      expect(b.id, 'turn-1:rstream', reason: 'HfyBlock.id is non-nullable in Dart; '
          'no fixture ever sends an envelope without an id, so the projection '
          'contract (which JS satisfies by DROPPING an undefined id key) never '
          'reaches this divergence');
    });

    test('a durable reasoning WITH an id replaces it exactly like ui.js:997', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev('r1', 'reasoning.delta', actor: agentActor, data: {'text': 'think'},
            when: at(0, 1)),
      );
      c.apply(
        ev('r2', 'reasoning', actor: agentActor, data: {'text': 'the full thought'},
            when: at(0, 5)),
      );
      expect(c.t1.blocks.single.id, 'r2');
    });
  });
}
