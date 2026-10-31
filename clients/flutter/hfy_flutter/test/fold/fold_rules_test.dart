// Unit tests over the fold's tricky rules, independent of the shared
// fixtures: grouping math (P-FOLD-027), card/sheet liveness (P-FOLD-041/049),
// settle rules (P-FOLD-016), durable-event replacement semantics
// (P-FOLD-006/009/058) and the loadOlder merge's stateSeen rule (P-FOLD-043),
// plus the ask/card/attachment helpers that no fixture exercises with
// invalid shapes (P-FOLD-051/054/020).

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/model/ask.dart';
import 'package:hfy_flutter/src/model/card.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyActor, HfyEvent;
import 'package:hfy_flutter/src/model/fold.dart' show HfyConversation;
import 'package:hfy_flutter/src/model/state.dart';

// 2026-01-02 10:00-based UTC stamps, min-granular for the 5-minute rule.
DateTime at(int min, [int s = 0]) => DateTime.utc(2026, 1, 2, 10, min, s);
const u1 = 'u1';
final userActor = HfyActor(uuid: u1, type: 'external', ref: u1);
final agentActor = HfyActor(uuid: 'agent', type: 'agent', ref: 'main');
final user2Actor = HfyActor(uuid: 'u2', type: 'external', ref: 'u2');

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
}) {
  final c = HfyConversation(
    viewer: viewer ?? const HfyViewer(uuid: u1, type: 'external', ref: u1),
    toolCalls: mode,
    base: Uri.parse('https://embed.example/'),
  );
  c.markLoaded();
  return c;
}

/// A starter `turn` event for turn-1 in the default scenario.
void startTurn(HfyConversation c) {
  c.apply(ev('turn-1', 'turn', parent: '', data: {'agent': 'main'}));
}

extension _Find on HfyConversation {
  HfyTurn get t1 => turns.firstWhere((t) => t.id == 'turn-1');
  HfyBlock byId(String id) => t1.blocks.firstWhere((b) => b.id == id);
  HfyBlock toolOf() => t1.blocks.lastWhere((b) => b.isTool);
}

void main() {
  group('grouping math (P-FOLD-027)', () {
    test(
      'a text block more than 5 minutes after the PREVIOUS VISIBLE block is a head',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev('b1', 'message.text', actor: agentActor, data: {'text': 'a'}),
        );
        c.apply(
          ev(
            'b2',
            'message.text',
            actor: agentActor,
            data: {'text': 'b'},
            when: at(5, 1), // 301s after b1 → a new group
          ),
        );
        c.apply(
          ev(
            'b3',
            'message.text',
            actor: agentActor,
            data: {'text': 'c'},
            when: at(5, 30), // same group
          ),
        );
        expect(c.byId('b1').showDay, isTrue);
        expect(c.byId('b1').showHead, isTrue);
        expect(c.byId('b2').showHead, isTrue);
        expect(c.byId('b2').showDay, isFalse);
        expect(c.byId('b3').showHead, isFalse);
      },
    );

    test(
      'a VISIBLE 4-second-old tool resets the gap (no head on same-actor text)',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev('b1', 'message.text', actor: agentActor, data: {'text': 'a'}),
        );
        c.apply(
          ev(
            'b2',
            'tool_call',
            actor: agentActor,
            data: {'name': 'x', 'call_id': 'c1'},
            when: at(4, 59),
          ),
        );
        c.apply(
          ev(
            'b3',
            'message.text',
            actor: agentActor,
            data: {'text': 'b'},
            when: at(5, 3),
          ),
        );
        // summary mode keeps the tool visible: prevTs is the tool's stamp
        expect(c.byId('b3').showHead, isFalse);
      },
    );

    test('hidden tool rows neither show a separator nor reset the clock', () {
      final c = foldOf(mode: HfyToolCalls.hidden);
      startTurn(c);
      c.apply(ev('b1', 'message.text', actor: agentActor, data: {'text': 'a'}));
      c.apply(
        ev(
          'b2',
          'tool_call',
          actor: agentActor,
          data: {'name': 'x', 'call_id': 'c1'},
          when: at(4, 59),
        ),
      );
      final tool = c.byId('b2');
      expect(tool.hidden, isTrue);
      expect(tool.showDay, isFalse);
      expect(tool.showHead, isFalse);
      c.apply(
        ev(
          'b3',
          'message.text',
          actor: agentActor,
          data: {'text': 'b'},
          when: at(5, 3),
        ),
      );
      // the hidden tool did not update prevTs → head by the 5-minute gap
      expect(c.byId('b3').showHead, isTrue);
    });

    test(
      'a calendar-day change in the zone opens a separator on the first block',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(ev('b1', 'message.text', data: {'text': 'a'}));
        c.apply(
          ev('b2', 'message.text', data: {'text': 'b'}, when: at(24 * 60, 5)),
        );
        final b2 = c.byId('b2');
        expect(b2.dayKey, '2026-01-03'); // UTC-pinned zone
        expect(b2.showDay, isTrue);
        expect(b2.showHead, isTrue); // a day change is a head too
      },
    );

    test(
      'same actor same day: exactly 5 minutes still groups (not > 5min)',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev('b1', 'message.text', actor: agentActor, data: {'text': 'a'}),
        );
        c.apply(
          ev(
            'b2',
            'message.text',
            actor: agentActor,
            data: {'text': 'b'},
            when: at(5, 0),
          ),
        );
        expect(c.byId('b2').showHead, isFalse);
      },
    );
  });

  group('card liveness (P-FOLD-041)', () {
    HfyConversation cardOnTurn1({HfyViewer? viewer}) {
      final c = foldOf(viewer: viewer);
      startTurn(c);
      c.apply(ev('b1', 'message.text', data: {'text': 'buy pizza'}));
      c.apply(
        ev(
          'tb',
          'tool_call',
          actor: agentActor,
          data: {
            'name': 'shop.pay',
            'call_id': 'c1',
            'arguments': <String, Object?>{},
          },
        ),
      );
      c.apply(
        ev(
          'tr',
          'tool_result',
          actor: agentActor,
          data: {
            'call_id': 'c1',
            'content': 'ok',
            'ui': {'v': 1, 'type': 'card', 'title': 'Payment', 'body': 'x'},
          },
        ),
      );
      c.apply(
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'completed'}),
      );
      return c;
    }

    test('live on the newest own turn while nothing of the viewer runs', () {
      final c = cardOnTurn1();
      expect(c.ownActive, isFalse);
      expect(c.toolOf().cardButtonsLive, isTrue);
    });

    test('a queued second own turn keeps ownActive on → all cards dead', () {
      final c = cardOnTurn1();
      c.apply(ev('turn-2', 'turn', parent: '', data: {'agent': 'main'}));
      c.apply(
        ev(
          't2m',
          'message.text',
          parent: 'turn-2',
          data: {'text': 'another one'},
        ),
      );
      expect(c.ownActive, isTrue); // turn-2 owns a user text and stays queued
      expect(c.toolOf().cardButtonsLive, isFalse);
    });

    test("a nonowner's card is never live", () {
      final c = cardOnTurn1(
        viewer: HfyViewer(uuid: 'u2', type: 'external', ref: 'u2'),
      );
      expect(c.turns.first.own, isFalse);
      expect(c.toolOf().cardButtonsLive, isFalse);
    });

    test('an awaiting_approval OWN turn keeps ownActive on', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('b1', 'message.text', data: {'text': 'book a table'}));
      c.apply(
        ev(
          'tb',
          'tool_call',
          actor: agentActor,
          data: {'name': 'shop.pay', 'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'ar',
          'approval.requested',
          actor: agentActor,
          data: {'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          's1',
          'turn.state',
          actor: agentActor,
          data: {'state': 'awaiting_approval'},
        ),
      );
      expect(c.ownActive, isTrue);
    });
  });

  group('settle rules (P-FOLD-016)', () {
    test(
      'a terminal turn settles an open approval: stopped, decided, interrupted',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(ev('b1', 'message.text', data: {'text': 'pay EUR 9'}));
        c.apply(
          ev(
            'tb',
            'tool_call',
            actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'},
          ),
        );
        c.apply(
          ev(
            'ar',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': 'c1'},
          ),
        );
        final tool = c.toolOf();
        expect(tool.showApproval, isTrue);
        expect(c.pendingOwn, ['c1']);
        expect(c.sheetOpen, isTrue);
        c.apply(
          ev(
            's',
            'turn.state',
            actor: agentActor,
            data: {'state': 'cancelled'},
          ),
        );
        expect(tool.stopped, isTrue);
        expect(tool.showApproval, isFalse);
        expect(tool.showDecided, isTrue);
        expect(tool.status, 'interrupted'); // ran before settling
        expect(c.pendingOwn, isEmpty);
        expect(c.sheetOpen, isFalse);
      },
    );

    test('a decision arriving AFTER the settle still overrides it', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev(
          'tb',
          'tool_call',
          actor: agentActor,
          data: {'name': 'shop.pay', 'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'ar',
          'approval.requested',
          actor: agentActor,
          data: {'call_id': 'c1'},
        ),
      );
      c.apply(
        ev('s', 'turn.state', actor: agentActor, data: {'state': 'stopped'}),
      );
      c.apply(
        ev(
          'ad',
          'approval.decided',
          actor: agentActor,
          data: {'call_id': 'c1', 'approve': true},
        ),
      );
      final tool = c.toolOf();
      // ui.js:1038: `showApproval/showDecided/decision` set again; the settle
      // flags stay (settle never checks stopped, decided never checks it)
      expect(tool.decision, 'allowed');
      expect(tool.showDecided, isTrue);
      expect(tool.stopped, isTrue);
    });

    test("a held call's tool_result settles it without a decision event", () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('b1', 'message.text', data: {'text': 'pay the invoice'}));
      c.apply(
        ev(
          'tb',
          'tool_call',
          actor: agentActor,
          data: {'name': 'shop.pay', 'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'ar',
          'approval.requested',
          actor: agentActor,
          data: {'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'tr',
          'tool_result',
          actor: agentActor,
          data: {'call_id': 'c1', 'content': 'paid'},
        ),
      );
      final tool = c.toolOf(); // found and settled by call_id
      expect(tool.callId, 'c1');
      expect(tool.showApproval, isFalse);
      expect(tool.showDecided, isTrue);
      expect(tool.stopped, isTrue);
      expect(tool.decision, '');
    });

    test(
      'an approval.requested after a terminal turn settles at once (late request)',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev(
            's',
            'turn.state',
            actor: agentActor,
            data: {'state': 'cancelled'},
          ),
        );
        c.apply(
          ev(
            'tb',
            'tool_call',
            actor: agentActor,
            data: {'name': 'hfy.ask_user', 'call_id': 'a1'},
          ),
        );
        c.apply(
          ev(
            'ar',
            'approval.requested',
            actor: agentActor,
            data: {
              'call_id': 'a1',
              'questions': <String, Object?>{'questions': <Object?>[]},
            },
          ),
        );
        final tool = c.toolOf();
        expect(tool.isAsk, isTrue);
        expect(tool.showApproval, isFalse);
        expect(tool.stopped, isTrue);
        // no questions in the call arguments → not a question set: askState
        // stays unset, exactly like ui.js (hfyAskQuestions returned []).
        expect(tool.askState, isNull);
      },
    );

    test('settling is idempotent', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev(
          'tb',
          'tool_call',
          actor: agentActor,
          data: {'name': 'shop.pay', 'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'ar',
          'approval.requested',
          actor: agentActor,
          data: {'call_id': 'c1'},
        ),
      );
      c.apply(
        ev(
          'tr',
          'tool_result',
          actor: agentActor,
          data: {'call_id': 'c1', 'content': 'x'},
        ),
      );
      // a second settle sweep (e.g. through the terminal state) changes nothing
      c.apply(
        ev('s', 'turn.state', actor: agentActor, data: {'state': 'completed'}),
      );
      final tool = c.toolOf();
      expect(tool.status, 'done'); // NOT re-interrupted
      expect(tool.stopped, isTrue);
    });
  });

  group('durable replacement (P-FOLD-006/009/058)', () {
    test(
      'the durable agent text replaces the streaming block and restamps',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev(
            'd1',
            'text.delta',
            actor: agentActor,
            data: {'text': 'Hel'},
            when: at(0, 10),
          ),
        );
        c.apply(
          ev(
            'd2',
            'text.delta',
            actor: agentActor,
            data: {'text': 'lo'},
            when: at(0, 12),
          ),
        );
        final streaming = c.byId('turn-1:stream');
        expect(streaming.text, 'Hello');
        expect(streaming.streaming, isTrue);
        c.apply(
          ev(
            'm',
            'message.text',
            actor: agentActor,
            data: {'text': 'Hello, world!'},
            when: at(0, 13),
          ),
        );
        final b = c.byId('m');
        expect(b.text, 'Hello, world!');
        expect(b.streaming, isFalse);
        expect(b.id, 'm');
        expect(b.iso, '2026-01-02T10:00:13.000Z');
        expect(c.t1.blocks.length, 1); // replaced in place
        // a NEW delta starts a fresh stream block
        c.apply(
          ev(
            'd4',
            'text.delta',
            actor: agentActor,
            data: {'text': ' more'},
            when: at(0, 14),
          ),
        );
        expect(c.t1.blocks.length, 2);
        expect(c.byId('turn-1:stream').text, ' more');
        expect(c.byId('turn-1:stream').streaming, isTrue);
      },
    );

    test('user text never replaces a streaming agent block', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('d1', 'text.delta', actor: agentActor, data: {'text': 'x'}));
      c.apply(ev('m', 'message.text', data: {'text': 'user text'}));
      final h = c.t1.blocks;
      expect(h, hasLength(2));
      expect(h[0].id, 'turn-1:stream');
      expect(h[1].id, 'm');
      expect(h[1].isUser, isTrue);
    });

    test(
      'reasoning replacement keeps its FIRST delta timestamps (P-FOLD-058)',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev(
            'r1',
            'reasoning.delta',
            actor: agentActor,
            data: {'text': 'think'},
            when: at(0, 1),
          ),
        );
        c.apply(
          ev(
            'r2',
            'reasoning',
            actor: agentActor,
            data: {'text': 'the full thought'},
            when: at(0, 5),
          ),
        );
        final b = c.byId('r2');
        expect(b.text, 'the full thought');
        expect(b.streaming, isFalse);
        expect(
          b.iso,
          '2026-01-02T10:00:01.000Z',
        ); // NOT restamped (ui.js:997 only)
        expect(c.t1.blocks.length, 1);
      },
    );
  });

  group('loadOlder merge rules (P-FOLD-043)', () {
    test(
      'a collected turn without its own turn.state never overwrites the state',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(ev('b1', 'message.text', data: {'text': 'live'}));
        c.apply(
          ev('s', 'turn.state', actor: agentActor, data: {'state': 'running'}),
        );
        // an older slice of the SAME turn, no turn.state inside
        c.applyHistoryPage([
          ev('old1', 'message.text', data: {'text': 'earlier'}, when: at(0)),
        ]);
        c.mergeOlder(hasMore: false);
        expect(c.t1.state, 'running'); // collector default must NOT win
        expect(c.t1.blocks.map((b) => b.id), ['b1', 'old1']);
        expect(c.hasOlder, isFalse);
      },
    );

    test(
      'a collected stateSeen turn overwrites the state and settles asks',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(ev('b1', 'message.text', data: {'text': 'size please'}));
        c.apply(
          ev(
            'tb',
            'tool_call',
            actor: agentActor,
            data: {'name': 'hfy.ask_user', 'call_id': 'a1'},
          ),
        );
        c.apply(
          ev(
            'ar',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': 'a1'},
          ),
        );
        expect(c.pendingOwn, ['a1']);
        c.applyHistoryPage([
          ev('s2', 'turn.state', actor: agentActor, data: {'state': 'stopped'}),
        ]);
        c.mergeOlder(hasMore: false);
        final tool = c.toolOf();
        expect(tool.stopped, isTrue);
        // the merged state event settles the held ask; askState stays unset —
        // this ask call carries no questions of its own (not a question set)
        expect(tool.askState, isNull);
        expect(c.pendingOwn, isEmpty);
      },
    );

    test('brand-new collected turns prepend in delivery order', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('m', 'message.text', data: {'text': 'newer'}));
      c.applyHistoryPage([
        ev('turn-0', 'turn', parent: '', data: {'agent': 'main'}),
        ev(
          'm0',
          'message.text',
          parent: 'turn-0',
          data: {'text': 'older turn'},
        ),
      ]);
      c.mergeOlder(hasMore: true);
      expect(c.turns.map((t) => t.id), ['turn-0', 'turn-1']);
      expect(c.hasOlder, isTrue);
      c.applyHistoryPage(const <HfyEvent>[]);
      c.mergeOlder(hasMore: false);
      expect(c.hasOlder, isFalse);
    });
  });

  group('sheet minimize/fresh state (P-FOLD-049)', () {
    test(
      'minimize closes the sheet, shows the pill; a new call surfaces it',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(ev('b1', 'message.text', data: {'text': 'pay please'}));
        c.apply(
          ev(
            'tb',
            'tool_call',
            actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'},
          ),
        );
        c.apply(
          ev(
            'ar',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': 'c1'},
          ),
        );
        expect(c.sheetOpen, isTrue);
        expect(c.showActionPill, isFalse);
        c.minimizeSheet();
        c.relayout();
        c.syncDerived();
        expect(c.sheetOpen, isFalse);
        expect(c.showActionPill, isTrue);
        // a NEW pending call id surfaces the sheet again (_seenPending)
        c.apply(
          ev(
            'tb2',
            'tool_call',
            actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c2'},
          ),
        );
        c.apply(
          ev(
            'ar2',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': 'c2'},
          ),
        );
        expect(c.pendingOwn, ['c1', 'c2']);
        expect(c.sheetOpen, isTrue);
        expect(c.showActionPill, isFalse);
      },
    );

    test('decided calls leave pendingOwn in folded order', () {
      final c = foldOf();
      startTurn(c);
      c.apply(ev('b1', 'message.text', data: {'text': 'two pays'}));
      for (final cid in ['a', 'b']) {
        c.apply(
          ev(
            't$cid',
            'tool_call',
            actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': cid},
          ),
        );
        c.apply(
          ev(
            'r$cid',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': cid},
          ),
        );
      }
      expect(c.pendingOwn, ['a', 'b']);
      c.openSheet(); // reopen state is trivially stable here
      c.apply(
        ev(
          'ad',
          'approval.decided',
          actor: agentActor,
          data: {'call_id': 'a', 'approve': false},
        ),
      );
      expect(c.pendingOwn, ['b']);
    });
  });

  group('ask record helpers (P-FOLD-051/052/053)', () {
    const labels = HfyAnswerLabels(yes: 'Yes', no: 'No', none: 'No answer');

    test(
      'invalid questions drop out; required defaults true; labels fall back',
      () {
        final qs = hfyAskQuestions({
          'questions': [
            {'id': 'q1', 'type': 'double', 'text': 'bad'}, // invalid type
            {'type': 'text', 'text': 'no id'}, // missing id
            {
              'id': 'q2',
              'type': 'single',
              'options': [
                {'value': 'a'}, // label falls back to the value
                {'value': null}, // dropped
              ],
            },
            {
              'id': 'q3',
              'type': 'text',
              'required': false,
              'placeholder': 'hi',
            },
          ],
        });
        expect(qs.map((q) => q.id), ['q2', 'q3']);
        expect(qs[0].required, isTrue);
        expect(qs[0].options.map((o) => o.label), ['a']);
        expect(qs[1].required, isFalse);
        expect(qs[1].placeholder, 'hi');
      },
    );

    test('hfyAnswerOk shape rules', () {
      final q = hfyAskQuestions({
        'questions': [
          {
            'id': 's',
            'type': 'single',
            'options': [
              {'value': 'a'},
              {'value': 'b'},
            ],
          },
        ],
      }).first;
      expect(q.answerOk('a'), isTrue);
      expect(q.answerOk('c'), isFalse);
      expect(q.answerOk(null), isFalse); // required
      expect(q.answerOk(<String>['a']), isFalse); // not a string value
      final multi = hfyAskQuestions({
        'questions': [
          {
            'id': 'm',
            'type': 'multi',
            'options': [
              {'value': 'a'},
              {'value': 'b'},
            ],
          },
        ],
      }).first;
      expect(multi.answerOk(['a', 'b']), isTrue);
      expect(
        multi.answerOk(['b', 'a']),
        isTrue,
      ); // ui.js: order free, entries known
      expect(
        multi.answerOk(['a', 'a']),
        isFalse,
      ); // duplicates fail (ui.js:227)
      expect(multi.answerOk(['a', 'c']), isFalse); // unknown values fail
      expect(multi.answerOk([]), isFalse); // empty is the optional path only
      final optional = hfyAskQuestions({
        'questions': [
          {
            'id': 'o',
            'type': 'multi',
            'required': false,
            'options': [
              {'value': 'a'},
            ],
          },
        ],
      }).first;
      expect(optional.answerOk([]), isTrue);
      expect(optional.answerOk(null), isTrue);
      final text = hfyAskQuestions({
        'questions': [
          {'id': 't', 'type': 'text'},
        ],
      }).first;
      expect(text.answerOk(' pretending '), isTrue);
      expect(text.answerOk('x' * 2001), isFalse);
      expect(text.answerOk('x' * 2000), isTrue);
      final confirm = hfyAskQuestions({
        'questions': [
          {'id': 'c', 'type': 'confirm'},
        ],
      }).first;
      expect(confirm.answerOk(true), isTrue);
      expect(confirm.answerOk('true'), isFalse); // strictly boolean
    });

    test('hfyAnswerText words', () {
      final qs = hfyAskQuestions(const {
        'questions': [
          {'id': 'c', 'type': 'confirm'},
          {
            'id': 's',
            'type': 'single',
            'options': [
              {'value': 'a', 'label': 'Alpha'},
            ],
          },
        ],
      });
      expect(hfyAnswerText(qs[0], true, labels), 'Yes');
      expect(hfyAnswerText(qs[0], null, labels), 'No answer');
      expect(hfyAnswerText(qs[1], 'a', labels), 'Alpha');
      // unknown single values fall back to the raw words (ui.js:246-249)
      expect(hfyAnswerText(qs[1], 'zz', labels), 'zz');
    });

    test('hfyAskPayload drops empty answers, trims strings, copies arrays', () {
      final qs = hfyAskQuestions(const {
        'questions': [
          {'id': 'q', 'type': 'multi', 'required': false},
          {'id': 't', 'type': 'text', 'required': false},
        ],
      });
      final payload = hfyAskPayload(qs, {
        'q': ['a', 'b'],
        't': '  spaced  ',
        'skip': '',
      });
      expect(payload, {
        'q': ['a', 'b'],
        't': 'spaced',
      });
    });
  });

  group('card data (P-FOLD-054)', () {
    test('carousel → its renderable cards; single card; unusable → null', () {
      expect(
        hfyCardData({
          'type': 'carousel',
          'cards': [
            {'title': 'A', 'body': 'b'},
            {'nope': 1},
          ],
        })!.map((c) => c.title).toList(),
        ['A'],
      );
      expect(hfyCardData({'type': 'card', 'title': 'A'})!.length, 1);
      expect(hfyCardData({'type': 'card'}), isNull); // no title/body → null
      expect(hfyCardData('nope'), isNull);
      expect(hfyCardData(null), isNull);
    });
  });

  group('attachment mapping (P-FOLD-020)', () {
    test(
      'relative urls resolve against the base; unsafe schemes drop to plain',
      () {
        final atts = hfyMapAttachments(const [
          {'url': '/v1/files/x.png', 'mime': 'image/png', 'name': 'x.png'},
          {'url': 'javascript:alert(1)', 'mime': 'image/png', 'name': 'evil'},
          {'mime': 'application/pdf', 'name': 'p.pdf'},
        ], base: Uri.parse('https://embed.example/'));
        expect(atts[0].url, 'https://embed.example/v1/files/x.png');
        expect(atts[0].isImage, isTrue);
        expect(atts[1].url, '');
        expect(atts[1].isPlain, isTrue);
        expect(atts[2].isLink, isFalse); // no safe url → plain
        expect(atts[2].isPlain, isTrue);
        expect(atts[2].label, 'p.pdf');
      },
    );

    test('the label falls back to mime then the file string (ui.js:933)', () {
      final atts = hfyMapAttachments(const [
        {'mime': 'image/png', 'url': 'https://a/b.png'},
        {'url': 'https://a/c'},
      ], base: Uri.parse('https://embed.example/'));
      expect(atts[0].label, 'image/png');
      expect(atts[1].label, 'File');
      expect(atts[1].isLink, isTrue);
      // isPlain rows carry no url, exactly like the JS mapAtts
      expect(atts[0].isImage, isTrue);
      expect(atts[0].isLink, isFalse);
    });
  });

  group('welcome + typing', () {
    test(
      'an empty loaded conversation welcomes; turns silence it (P-FOLD-046)',
      () {
        final c = foldOf();
        c.syncDerived(); // the fixtures end every scenario with relayout + sync
        expect(c.showWelcome, isTrue);
        startTurn(c);
        expect(c.showWelcome, isFalse);
        expect(c.turns, hasLength(1));
      },
    );

    test('hasOlder keeps the welcome down while nothing folds', () {
      final c = foldOf();
      c.hasOlder = true; // the component's hasOlder only moves via loadOlder
      c.syncDerived();
      expect(c.showWelcome, isFalse);
    });

    test(
      'typing dots live on the first active turn with a non-busy tail (P-FOLD-036)',
      () {
        final c = foldOf();
        startTurn(c);
        c.apply(
          ev('s1', 'turn.state', actor: agentActor, data: {'state': 'running'}),
        );
        // the last block is the user's own text — nothing spins → dots on turn-1
        expect(c.turns.first.showTyping, isTrue);
        // a busy tail (an agent text block) kills them
        c.apply(
          ev('m2', 'message.text', actor: agentActor, data: {'text': 'a'}),
        );
        expect(c.turns.first.showTyping, isFalse);

        // an awaiting_approval tail is busy too
        final c2 = foldOf();
        startTurn(c2);
        c2.apply(
          ev(
            'tb',
            'tool_call',
            actor: agentActor,
            data: {'name': 'shop.pay', 'call_id': 'c1'},
          ),
        );
        c2.apply(
          ev(
            'ar',
            'approval.requested',
            actor: agentActor,
            data: {'call_id': 'c1'},
          ),
        );
        expect(c2.turns.first.showTyping, isFalse);
      },
    );

    test('only the FIRST active turn type dots (two-active rule)', () {
      final c = foldOf();
      startTurn(c);
      c.apply(
        ev('s1', 'turn.state', actor: agentActor, data: {'state': 'running'}),
      );
      c.apply(ev('turn-2', 'turn', parent: '', data: {'agent': 'main'}));
      expect(c.turns.first.showTyping, isTrue);
      final t2 = c.turns.firstWhere((t) => t.id == 'turn-2');
      expect(t2.showTyping, isFalse);
      // an awaiting_approval turn also consumes the firstActive slot
      final c2 = foldOf();
      startTurn(c2);
      c2.apply(
        ev(
          's1',
          'turn.state',
          actor: agentActor,
          data: {'state': 'awaiting_approval'},
        ),
      );
      c2.apply(ev('turn-2', 'turn', parent: '', data: {'agent': 'main'}));
      final first = c2.turns.first;
      final second = c2.turns.firstWhere((t) => t.id == 'turn-2');
      // awaiting_approval is NOT active but consumes firstActive anyway
      // (ui.js:1166) → neither turn shows the dots
      expect(first.showTyping, isFalse);
      expect(second.showTyping, isFalse);
    });
  });
}
