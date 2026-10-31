// HfyChatController over the real HfyClient and the fake server: connect and
// the fold, the composer send/stop flows, the one-active-turn rule, the
// approval sheet's submit and landing rules, loadOlder, retry, notification
// coalescing and disposal. Rows named by their PARITY IDs.
import 'dart:async';
import 'dart:convert' show jsonDecode, jsonEncode;

import 'package:flutter/foundation.dart'
    show FlutterError, FlutterErrorDetails, FlutterExceptionHandler;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart'
    show HfyClientOptions, HfyUploadsConfig;
import 'package:hfy_flutter/src/client/connection.dart' show HfyConnectionState;
import 'package:hfy_flutter/src/controller/chat_controller.dart';
import 'package:hfy_flutter/src/controller/message.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyProtocolException;

import '../client/test_kit.dart';
import 'controller_kit.dart';

ChatKit? _kit;

ChatKit get kit => _kit!;

void main() {
  late FakeHfyServer server;
  final reported = <FlutterErrorDetails>[];
  FlutterExceptionHandler? previousOnError;

  setUp(() async {
    reported.clear();
    previousOnError = FlutterError.onError;
    FlutterError.onError = reported.add;
    server = await FakeHfyServer.start();
  });

  tearDown(() async {
    _kit?.controller.dispose();
    _kit = null;
    await server.destroy();
    FlutterError.onError = previousOnError;
    expect(
      reported,
      isEmpty,
      reason: 'nothing may be reported to the framework',
    );
  });

  Future<void> openWith(
    FakeHfyServer s, {
    HfyChatOptions options = const HfyChatOptions(),
    HfyTokenProvider? provider,
  }) async {
    _kit = await ChatKit.open(s, options: options, provider: provider);
  }

  Map<String, Object?> emptyPage({String? next}) => historyPage(
    turns: const <Object?>[],
    next: next,
    liveAfter: 'la-0',
    viewer: kViewer,
  );

  group('connect and the fold', () {
    test(
      'P-CFG-021: connect loads the page, marks it loaded and reads hasOlder right after (P-CFG-028)',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t1', 'turn'), userText('m1', 't1', 'hi')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        expect(c.showWelcome, isFalse);
        await kit.connectLive();
        expect(c.connection, HfyConnectionState.live);
        expect(c.hasOlder, isTrue);
        expect(c.turns.single.blocks.single.text, 'hi');
        expect(c.showWelcome, isFalse);
      },
    );

    test(
      'P-FOLD-046: welcome shows only after a successful connect with no turns and no older page',
      () async {
        server.historyPages[null] = emptyPage();
        await openWith(server);
        final c = kit.controller;
        expect(c.showWelcome, isFalse, reason: 'no pre-connect flash');
        await c.connect();
        expect(c.showWelcome, isTrue);
      },
    );

    test(
      'P-FOLD-046: a failed connect records the server text and never shows the welcome',
      () async {
        await openWith(server); // no history page: the fake answers 404
        final c = kit.controller;
        await c.connect();
        expect(c.connection, HfyConnectionState.error);
        expect(c.connectionError, const HfyMessage.text('not found'));
        expect(c.showWelcome, isFalse);
      },
    );

    test(
      'P-FOLD-017: history folds as a page, live envelopes fold live',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([
              wire('t1', 'turn'),
              userText('m1', 't1', 'question'),
              agentText('m2', 't1', 'answer'),
            ]),
          ],
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        await kit.frame(agentText('m3', 't1', 'follow-up'));
        await settle();
        final texts = [for (final b in c.turns.single.blocks) b.text];
        expect(texts, ['question', 'answer', 'follow-up']);
        expect(c.liveEventSeq, 1, reason: 'only the live envelope counts');
      },
    );

    test(
      'P-FOLD-017: a history event without an id is history, never a live pill',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([
              wire('t1', 'turn'),
              userText('m1', 't1', 'question'),
              withoutId(agentText('m2', 't1', 'answer')),
              agentText('m3', 't1', 'follow-up'),
            ]),
          ],
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        c.stickToBottom.value = false;
        await c.connect();
        expect(
          [for (final b in c.turns.single.blocks) b.text],
          ['question', 'answer', 'follow-up'],
        );
        expect(
          c.newMessagesPending.value,
          isFalse,
          reason: 'history never raises the pill',
        );
        expect(c.liveEventSeq, 0, reason: 'no history event counts as live');
      },
    );

    test(
      'P-FOLD-022: a send made before connect keeps Stop armed through the boot',
      () async {
        server.historyPages[null] = emptyPage();
        server.turnForMessages = 't7';
        await openWith(server);
        final c = kit.controller;
        c.setDraft('early');
        await c.submitDraft();
        expect(c.ownActive, isTrue, reason: 'the bridge waits for the turn');
        await kit.connectLive();
        expect(
          c.ownActive,
          isTrue,
          reason: 'the bridge survives the conversation being created',
        );
        await kit.frame(wire('t7', 'turn'));
        await kit.frame(userText('m1', 't7', 'early'));
        await kit.frame(turnState('s1', 't7', 'completed'));
        await settle();
        expect(c.ownActive, isFalse);
      },
    );
  });

  group('send', () {
    setUp(() async {
      server.historyPages[null] = emptyPage();
      server.turnForMessages = 't9';
      await openWith(server);
      await kit.connectLive();
    });

    test(
      'P-FOLD-047: submit trims, clears the draft before the request, arms the bridge on success',
      () async {
        final c = kit.controller;
        c.setDraft('  hello  ');
        final pending = c.submitDraft();
        expect(c.draft, '', reason: 'cleared before the await');
        await pending;
        expect(server.messagesBodies.single, '{"text":"hello"}');
        expect(c.ownActive, isTrue);
        expect(c.canStop, isTrue);
        expect(c.canSend, isFalse);
        expect(c.composerError, isNull);
      },
    );

    test(
      'P-FOLD-047: a failed send restores the text and surfaces the server text',
      () async {
        server.scriptedErrors.add(('/v1/conversation/messages', 500, 'boom'));
        final c = kit.controller;
        c.setDraft('hi');
        await c.submitDraft();
        expect(c.draft, 'hi');
        expect(c.composerError, const HfyMessage.text('boom'));
        expect(
          c.ownActive,
          isFalse,
          reason: 'a failed send never arms the bridge',
        );
        expect(c.canSend, isTrue, reason: 'the user can retry');
      },
    );

    test(
      'P-CMP-004: a failure does not overwrite a draft the user typed meanwhile',
      () async {
        final hold = Completer<void>();
        server.holdRequests = hold;
        server.scriptedErrors.add(('/v1/conversation/messages', 500, 'boom'));
        final c = kit.controller;
        c.setDraft('first');
        final pending = c.submitDraft();
        await until(
          () => server.requests.any((r) => r.path.endsWith('/messages')),
        );
        c.setDraft('typed meanwhile');
        hold.complete();
        await pending;
        expect(c.draft, 'typed meanwhile');
        expect(c.composerError, const HfyMessage.text('boom'));
      },
    );

    test(
      'P-CMP-016 / P-CMP-018: a success clears the composer error; typing clears it too',
      () async {
        server.scriptedErrors.add(('/v1/conversation/messages', 500, 'boom'));
        final c = kit.controller;
        c.setDraft('one');
        await c.submitDraft();
        expect(c.composerError, isNotNull);
        c.setDraft('two');
        expect(
          c.composerError,
          isNull,
          reason: 'a draft change resets the error',
        );
        await c.submitDraft();
        expect(c.composerError, isNull, reason: 'the success clears it too');
      },
    );

    test(
      'P-CMP-015: a failure that is not an HTTP answer shows the localizable error key',
      () async {
        kit.controller.dispose();
        await openWith(
          server,
          provider: (reason) async {
            if (reason == 'unauthorized') throw StateError('no refresh');
            return 'tok';
          },
        );
        await kit.connectLive();
        server.schedule401();
        final c = kit.controller;
        c.setDraft('hi');
        await c.submitDraft();
        expect(c.composerError, const HfyMessage.key('error'));
        expect(c.textOf(c.composerError!), 'Something went wrong');
      },
    );

    test(
      'P-FOLD-056: every send forces stick-to-bottom and clears the pill before the request',
      () async {
        final c = kit.controller;
        c.stickToBottom.value = false;
        await kit.frame(agentText('m5', 't9', 'while detached'));
        await settle();
        expect(c.newMessagesPending.value, isTrue);
        final hold = Completer<void>();
        server.holdRequests = hold;
        c.setDraft('go');
        final pending = c.submitDraft();
        await until(
          () => server.requests.any((r) => r.path.endsWith('/messages')),
        );
        expect(c.stickToBottom.value, isTrue);
        expect(c.newMessagesPending.value, isFalse);
        hold.complete();
        await pending;
      },
    );

    test(
      'PARITY §10a #7: a send the server never echoes keeps Stop armed until stop()',
      () async {
        final c = kit.controller;
        c.setDraft('lost');
        await c.submitDraft();
        await kit.frame(wire('t9', 'turn'));
        await kit.frame(turnState('s1', 't9', 'completed'));
        await until(
          () => c.turns.any((t) => t.id == 't9' && t.state == 'completed'),
        );
        expect(c.ownActive, isTrue, reason: 'no user text landed for t9');
        expect(c.canStop, isTrue);
        c.setDraft('again');
        expect(c.canSend, isFalse, reason: 'Send waits on the bridge');
        await c.stop();
        expect(c.ownActive, isFalse);
        expect(c.canSend, isTrue);
        expect(kit.requestsTo('/v1/conversation/stop'), hasLength(1));
      },
    );
  });

  group('the one-active-turn rule', () {
    setUp(() async {
      server.historyPages[null] = emptyPage();
      await openWith(server);
      await kit.connectLive();
    });

    test(
      'P-CMP-003 / P-FOLD-022: the own turn gates Send while queued, running or awaiting approval',
      () async {
        final c = kit.controller;
        c.setDraft('next');
        await kit.frame(wire('t1', 'turn'));
        await kit.frame(userText('m1', 't1', 'hi'));
        await settle();
        expect(c.canSend, isFalse, reason: 'queued');
        expect(c.canStop, isTrue);
        await kit.frame(turnState('s1', 't1', 'running'));
        await settle();
        expect(c.canSend, isFalse, reason: 'running');
        await kit.frame(turnState('s2', 't1', 'awaiting_approval'));
        await settle();
        expect(c.canSend, isFalse, reason: 'awaiting approval');
        await kit.frame(turnState('s3', 't1', 'completed'));
        await settle();
        expect(c.canSend, isTrue, reason: 'completed frees the composer');
        expect(c.canStop, isFalse);
      },
    );

    test(
      'P-FOLD-022: another viewer\'s active turn does not block the viewer',
      () async {
        final c = kit.controller;
        c.setDraft('mine');
        await kit.frame(wire('t2', 'turn'));
        await kit.frame(userText('m2', 't2', 'from bob', uuid: 'u-bob'));
        await kit.frame(turnState('s1', 't2', 'running'));
        await settle();
        expect(c.ownActive, isFalse);
        expect(c.canSend, isTrue);
      },
    );
  });

  group('stop', () {
    setUp(() async {
      server.historyPages[null] = emptyPage();
      server.turnForMessages = 't9';
      await openWith(server);
      await kit.connectLive();
    });

    test(
      'P-FOLD-048 / P-CMP-014: stop drops the bridge at once, posts stop, clears the composer error',
      () async {
        final c = kit.controller;
        c.setDraft('go');
        await c.submitDraft();
        expect(c.ownActive, isTrue);
        final pending = c.stop();
        expect(
          c.ownActive,
          isFalse,
          reason: 'the bridge is dropped before the request',
        );
        await pending;
        expect(kit.requestsTo('/v1/conversation/stop'), hasLength(1));
        expect(c.composerError, isNull);
      },
    );

    test(
      'P-FOLD-048: a failed stop surfaces the server text, and a later stop clears it',
      () async {
        final c = kit.controller;
        server.scriptedErrors.add((
          '/v1/conversation/stop',
          409,
          'nothing to stop',
        ));
        await c.stop();
        expect(c.composerError, const HfyMessage.text('nothing to stop'));
        await c.stop();
        expect(c.composerError, isNull);
      },
    );
  });

  group('approval sheet', () {
    Future<void> askAndPlain() async {
      server.historyPages[null] = emptyPage();
      await openWith(server);
      await kit.connectLive();
      await kit.frame(wire('t1', 'turn'));
      await kit.frame(userText('m1', 't1', 'help'));
      await kit.frame(
        toolCall(
          'k1',
          't1',
          'c1',
          'hfy.ask_user',
          arguments: askArgs([
            textQuestion('q1'),
            singleQuestion('q2', ['a', 'b']),
          ]),
        ),
      );
      await kit.frame(approvalRequested('r1', 't1', 'c1'));
      await kit.frame(toolCall('k2', 't1', 'c2', 'shell'));
      await kit.frame(approvalRequested('r2', 't1', 'c2'));
      await kit.frame(turnState('s1', 't1', 'awaiting_approval'));
      await settle();
    }

    test(
      'P-FOLD-042 / P-FOLD-049: pending calls list in call order and open the sheet',
      () async {
        await askAndPlain();
        final c = kit.controller;
        expect([for (final b in c.pendingOwn) b.callId], ['c1', 'c2']);
        expect(c.sheetOpen, isTrue);
        expect(c.showActionPill, isFalse);
      },
    );

    test(
      'P-SH-013 / P-FOLD-055: decide posts in call order with the ask answers (approve true + answers)',
      () async {
        await askAndPlain();
        final c = kit.controller;
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {'q1': 'ok', 'q2': 'a'},
            questionIds: ['q1', 'q2'],
          ),
          HfyDecision(callId: 'c2', approve: false),
        ]);
        final bodies = [
          for (final r in kit.requestsTo('/v1/conversation/approvals'))
            jsonDecode(r.body),
        ];
        expect(bodies, [
          {
            'call_id': 'c1',
            'approve': true,
            'answers': {'q1': 'ok', 'q2': 'a'},
          },
          {'call_id': 'c2', 'approve': false},
        ]);
        expect(c.sheetBusy, isFalse);
        expect(c.sheetErrorKey, isNull);
      },
    );

    test(
      'P-SH-014: a 400 naming a question lands the sheet on that question',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'answer for q2 is not an option',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {'q1': 'x'},
            questionIds: ['q1', 'q2'],
          ),
        ]);
        expect(c.sheetErrorKey, 'c1:q2');
        expect(
          c.sheetError,
          const HfyMessage.text('answer for q2 is not an option'),
        );
        expect(c.sheetBusy, isFalse);
        expect(
          [for (final b in c.pendingOwn) b.callId],
          ['c1', 'c2'],
          reason: 'nothing was submitted',
        );
      },
    );

    test(
      'P-SH-014: the named question matches as a whole word, never as a regex (ui.js:1648 deviation)',
      () async {
        await askAndPlain();
        final c = kit.controller;
        // A regex built from q.1 matches "qx1"; the whole-word search does not.
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'qx1 is wrong',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {},
            questionIds: ['q.1', 'qx1'],
          ),
        ]);
        expect(c.sheetErrorKey, 'c1:qx1');
        // "q1" sits inside "q10": only the whole id "q10" names the question.
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'q10 is wrong',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {},
            questionIds: ['q1', 'q10'],
          ),
        ]);
        expect(c.sheetErrorKey, 'c1:q10');
      },
    );

    test(
      'P-FOLD-055: the first failure stops the run and the error lands on the failing call',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add(('/v1/conversation/approvals', 400, 'nope'));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {'q1': 'x'},
            questionIds: ['q1', 'q2'],
          ),
          HfyDecision(callId: 'c2', approve: true),
        ]);
        expect(kit.requestsTo('/v1/conversation/approvals'), hasLength(1));
        expect(
          c.sheetErrorKey,
          'c1:q1',
          reason: 'no name in the text: the first question',
        );
      },
    );

    test(
      'P-SH-014: sheetBusy is true while the decisions are in flight',
      () async {
        await askAndPlain();
        final c = kit.controller;
        final hold = Completer<void>();
        server.holdRequests = hold;
        final pending = c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c2', approve: true),
        ]);
        await until(
          () => server.requests.any((r) => r.path.endsWith('/approvals')),
        );
        expect(c.sheetBusy, isTrue);
        hold.complete();
        await pending;
        expect(c.sheetBusy, isFalse);
      },
    );

    test(
      'P-FOLD-042: submitted calls leave the sheet while their decided events are still in flight',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add(('/v1/conversation/approvals', 202, ''));
        server.scriptedErrors.add(('/v1/conversation/approvals', 202, ''));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {'q1': 'x'},
            questionIds: ['q1', 'q2'],
          ),
          HfyDecision(callId: 'c2', approve: true),
        ]);
        expect(c.pendingOwn, isEmpty);
        expect(c.sheetOpen, isFalse);
        await kit.frame(approvalDecided('d1', 't1', 'c1', approve: true));
        await kit.frame(approvalDecided('d2', 't1', 'c2', approve: true));
        await settle();
        expect(c.pendingOwn, isEmpty);
        await kit.frame(toolCall('k3', 't1', 'c3', 'shell'));
        await kit.frame(approvalRequested('r3', 't1', 'c3'));
        await settle();
        expect(
          [for (final b in c.pendingOwn) b.callId],
          ['c3'],
          reason: 'a fresh call reopens the sheet',
        );
        expect(c.sheetOpen, isTrue);
      },
    );

    test(
      'P-SH-013: skipQuestions posts approve false and shows a failure without landing the sheet',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          500,
          'skip failed',
        ));
        await c.skipQuestions('c1');
        expect(c.sheetError, const HfyMessage.text('skip failed'));
        expect(c.sheetErrorKey, isNull);
        await c.skipQuestions('c1');
        final bodies = [
          for (final r in kit.requestsTo('/v1/conversation/approvals'))
            jsonDecode(r.body),
        ];
        expect(bodies.last, {'call_id': 'c1', 'approve': false});
        expect(c.sheetError, isNull);
      },
    );

    test(
      'P-SH-015 / P-FOLD-049: minimizing keeps the calls pending; a new request reopens the sheet',
      () async {
        await askAndPlain();
        final c = kit.controller;
        c.minimizeSheet();
        expect(c.sheetOpen, isFalse);
        expect(c.showActionPill, isTrue);
        expect(c.pendingOwn, hasLength(2));
        await kit.frame(toolCall('k3', 't1', 'c3', 'shell'));
        await kit.frame(approvalRequested('r3', 't1', 'c3'));
        await settle();
        expect(c.sheetOpen, isTrue);
        expect(c.showActionPill, isFalse);
      },
    );

    test(
      'P-FOLD-042: draft keystrokes leave the sheet view as it was',
      () async {
        await askAndPlain();
        final c = kit.controller;
        final pending = c.pendingOwn;
        final revision = c.revision;
        for (final text in ['a', 'ab', 'abc']) {
          c.setDraft(text);
        }
        expect(
          identical(c.pendingOwn, pending),
          isTrue,
          reason: 'the fold did not relayout',
        );
        expect(c.revision, revision);
        expect(c.sheetOpen, isTrue);
        expect(() => c.pendingOwn.clear(), throwsUnsupportedError);
      },
    );

    test(
      'P-SH-014: a decision made while a submit is in flight posts nothing',
      () async {
        await askAndPlain();
        final c = kit.controller;
        final hold = Completer<void>();
        server.holdRequests = hold;
        addTearDown(() {
          if (!hold.isCompleted) hold.complete();
        });
        final first = c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c2', approve: true),
        ]);
        await until(
          () => server.requests.any((r) => r.path.endsWith('/approvals')),
        );
        expect(c.sheetBusy, isTrue);
        // c1 is still open, so only the busy guard can stop this one. It must
        // return at once, while the first submit is still held.
        final second = c.decide([
          const HfyDecision(callId: 'c1', approve: true, answers: {}),
        ]);
        await second.timeout(
          const Duration(seconds: 5),
          onTimeout: () => fail('a busy sheet ignores a second submit at once'),
        );
        expect(
          kit.requestsTo('/v1/conversation/approvals'),
          hasLength(1),
          reason: 'the busy sheet posted nothing for the second submit',
        );
        hold.complete();
        await first;
        expect(c.sheetBusy, isFalse);
      },
    );

    test(
      'P-FOLD-042: a decision for a call already submitted posts nothing',
      () async {
        await askAndPlain();
        final c = kit.controller;
        await c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c2', approve: true),
        ]);
        await c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c2', approve: false),
        ]);
        await c.skipQuestions('c2');
        expect(kit.requestsTo('/v1/conversation/approvals'), hasLength(1));
        expect([for (final b in c.pendingOwn) b.callId], ['c1']);
      },
    );

    test(
      'P-SH-014: an ask sent without question ids fails on the question the server names, else its first',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'q2 is not an option',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c1', approve: true, answers: {'q1': 'x'}),
        ]);
        expect(c.sheetErrorKey, 'c1:q2');
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'nope',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(callId: 'c1', approve: true, answers: {'q1': 'x'}),
        ]);
        expect(c.sheetErrorKey, 'c1:q1', reason: 'no name: the first question');
      },
    );

    test(
      'P-SH-014: a failed landing is dropped once its calls are all gone',
      () async {
        await askAndPlain();
        final c = kit.controller;
        server.scriptedErrors.add((
          '/v1/conversation/approvals',
          400,
          'q2 bad',
        ));
        await c.decide(const <HfyDecision>[
          HfyDecision(
            callId: 'c1',
            approve: true,
            answers: {},
            questionIds: ['q1', 'q2'],
          ),
        ]);
        expect(c.sheetErrorKey, 'c1:q2');
        await kit.frame(approvalDecided('d1', 't1', 'c1', approve: false));
        await kit.frame(approvalDecided('d2', 't1', 'c2', approve: false));
        await until(() => c.pendingOwn.isEmpty);
        expect(c.sheetErrorKey, isNull);
        expect(c.sheetError, isNull);
      },
    );

    test('P-SH-014: clearSheetError drops only an active landing', () async {
      await askAndPlain();
      final c = kit.controller;
      server.scriptedErrors.add(('/v1/conversation/approvals', 400, 'q2 bad'));
      await c.decide(const <HfyDecision>[
        HfyDecision(
          callId: 'c1',
          approve: true,
          answers: {},
          questionIds: ['q1', 'q2'],
        ),
      ]);
      expect(c.sheetErrorKey, 'c1:q2');
      c.clearSheetError();
      expect(c.sheetErrorKey, isNull);
      expect(c.sheetError, isNull);
    });
  });

  group('loadOlder', () {
    test(
      'P-FOLD-043 / P-TL-003: loadOlder merges the older page once and clears hasOlder at the end',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        server.historyPages['c1'] = historyPage(
          turns: [
            pageTurn([wire('t1', 'turn'), agentText('m1', 't1', 'oldest')]),
          ],
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        expect(c.hasOlder, isTrue);
        await c.loadOlder();
        expect([for (final t in c.turns) t.id], ['t1', 't2']);
        expect(c.hasOlder, isFalse);
        expect(c.loadingOlder, isFalse);
        expect(c.loadOlderError, isNull);
      },
    );

    test(
      'P-TL-003: a failed loadOlder keeps the turns and hasOlder and records the error',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        await openWith(server); // no 'c1' page: the fake answers 404
        final c = kit.controller;
        await kit.connectLive();
        await c.loadOlder();
        expect([for (final t in c.turns) t.id], ['t2']);
        expect(c.hasOlder, isTrue);
        expect(c.loadingOlder, isFalse);
        expect(c.loadOlderError, const HfyMessage.text('not found'));
      },
    );

    test(
      'P-FOLD-017: a live envelope arriving during loadOlder is applied at once; the page merges after',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        server.historyPages['c1'] = historyPage(
          turns: [
            pageTurn([wire('t1', 'turn'), agentText('m1', 't1', 'oldest')]),
          ],
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        final hold = Completer<void>();
        server.holdRequests = hold;
        final pending = c.loadOlder();
        await until(
          () => server.requests.any((r) => r.path.contains('before=c1')),
        );
        await kit.frame(agentText('m9', 't2', 'live during page'));
        await until(
          () => c.turns.last.blocks.any((b) => b.text == 'live during page'),
        );
        expect(c.loadingOlder, isTrue);
        hold.complete();
        await pending;
        expect([for (final t in c.turns) t.id], ['t1', 't2']);
        expect(
          [for (final b in c.turns.last.blocks) b.text],
          ['newest', 'live during page'],
        );
      },
    );

    test(
      'P-FOLD-043: an id-less event in an older page merges at the front, with no pill',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        server.historyPages['c1'] = historyPage(
          turns: [
            pageTurn([
              wire('t1', 'turn'),
              withoutId(agentText('m1', 't1', 'oldest')),
            ]),
          ],
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        c.stickToBottom.value = false;
        await c.loadOlder();
        expect([for (final t in c.turns) t.id], ['t1', 't2']);
        expect([for (final b in c.turns.first.blocks) b.text], ['oldest']);
        expect(c.newMessagesPending.value, isFalse);
        expect(c.liveEventSeq, 0);
      },
    );

    test(
      'P-CFG-021: connect() waits for an in-flight loadOlder before it boots',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        server.historyPages['c1'] = historyPage(
          turns: [
            pageTurn([wire('t1', 'turn'), agentText('m1', 't1', 'oldest')]),
          ],
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        // A paused client's connect() is a real boot, not a no-op on the open
        // stream.
        c.pause();
        final hold = Completer<void>();
        server.holdRequests = hold;
        addTearDown(() {
          if (!hold.isCompleted) hold.complete();
        });
        final older = c.loadOlder();
        await until(
          () => server.requests.any((r) => r.path.contains('before=c1')),
        );
        final boot = c.connect();
        // A boot that started would already show `connecting`: the client sets
        // it synchronously, before its first request.
        expect(
          c.connection,
          HfyConnectionState.paused,
          reason: 'the boot has not started: it waits for the older page',
        );
        hold.complete();
        await older;
        await boot;
        expect(c.connection, HfyConnectionState.live);
        expect([for (final t in c.turns) t.id], ['t1', 't2']);
      },
    );
  });

  group('retry', () {
    test(
      'P-CFG-022: retry reconnects after a failed connect and is a no-op while live',
      () async {
        await openWith(server); // no page yet: connect fails
        final c = kit.controller;
        await c.connect();
        expect(c.connectionError, isNotNull);
        server.historyPages[null] = emptyPage();
        await c.retry();
        await kit.kit.waitLive();
        expect(c.connection, HfyConnectionState.live);
        expect(c.connectionError, isNull);
        expect(c.showWelcome, isTrue, reason: 'the retry re-reads the page');
        final historyCount = kit.requestsTo('/v1/conversation/history').length;
        await c.retry();
        expect(
          kit.requestsTo('/v1/conversation/history'),
          hasLength(historyCount),
          reason: 'retry while live sends nothing',
        );
      },
    );
  });

  group('notifications and coalescing', () {
    test(
      'P-FOLD-017: 1000 history envelopes notify listeners a handful of times, not 1000',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([
              wire('t1', 'turn'),
              for (var i = 0; i < 1000; i++) agentText('h$i', 't1', 'm$i'),
            ]),
          ],
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        var notes = 0;
        c.addListener(() => notes++);
        await c.connect();
        await settle(20);
        expect(c.turns.single.blocks, hasLength(1000));
        expect(notes, lessThan(10));
      },
    );

    test(
      'P-FOLD-017: a burst of 1000 live frames coalesces into far fewer notifications',
      () async {
        server.historyPages[null] = emptyPage();
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        var notes = 0;
        c.addListener(() => notes++);
        final burst = StringBuffer()..write(_frameText(wire('t1', 'turn')));
        for (var i = 0; i < 1000; i++) {
          burst.write(_frameText(agentText('L$i', 't1', 'x$i')));
        }
        await kit.sse.write(burst.toString());
        await until(
          () => c.turns.isNotEmpty && c.turns.single.blocks.length == 1000,
        );
        await settle(20);
        expect(c.turns.single.blocks, hasLength(1000));
        expect(notes, lessThan(50));
      },
    );

    test(
      'P-FOLD-015: an unhandled live type (model.usage) never notifies',
      () async {
        server.historyPages[null] = emptyPage();
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        var notes = 0;
        c.addListener(() => notes++);
        final revision = c.revision;
        await kit.frame(
          wire('mu1', 'model.usage', data: <String, Object?>{'tokens': 3}),
        );
        // The stream is ordered: once this marker folds, mu1 has been handled.
        await kit.frame(wire('t1', 'turn'));
        await until(() => c.revision != revision);
        expect(notes, 1, reason: 'only the marker notified');
        expect(c.revision, revision + 1, reason: 'mu1 changed nothing');
      },
    );

    test(
      'P-FOLD-018: a live envelope while detached raises the pill; history never does',
      () async {
        server.historyPages[null] = historyPage(
          turns: [
            pageTurn([wire('t2', 'turn'), agentText('m3', 't2', 'newest')]),
          ],
          next: 'c1',
          liveAfter: 'la-0',
          viewer: kViewer,
        );
        server.historyPages['c1'] = historyPage(
          turns: [
            pageTurn([wire('t1', 'turn'), agentText('m1', 't1', 'oldest')]),
          ],
          viewer: kViewer,
        );
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        c.stickToBottom.value = false;
        await c.loadOlder();
        expect(
          c.newMessagesPending.value,
          isFalse,
          reason: 'history never raises the pill',
        );
        await kit.frame(agentText('m4', 't2', 'news'));
        await settle();
        expect(c.newMessagesPending.value, isTrue);
        c.stickToBottom.value = true;
        expect(
          c.newMessagesPending.value,
          isFalse,
          reason: 'back at the bottom clears it',
        );
      },
    );

    test(
      'listener re-entrancy: a listener that writes state during a notification leaves a consistent draft',
      () async {
        await openWith(server);
        final c = kit.controller;
        var calls = 0;
        c.addListener(() {
          calls++;
          if (calls == 1) {
            c.setDraft('typed');
            c.minimizeSheet();
          }
        });
        c.setDraft('first');
        await settle();
        expect(c.draft, 'typed');
        expect(calls, 2);
      },
    );

    test(
      'client review item 3 (no matrix row): a rejected envelope surfaces through protocolErrors and later frames still deliver',
      () async {
        server.historyPages[null] = emptyPage();
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        final errors = <HfyProtocolException>[];
        final sub = c.protocolErrors.listen(errors.add);
        addTearDown(sub.cancel);
        await kit.frame(<String, Object?>{
          'id': 'bad1',
          'type': 'message.text',
          'actor': 'oops',
          'data': <String, Object?>{},
        });
        await kit.frame(wire('t1', 'turn'));
        await kit.frame(agentText('m1', 't1', 'after the bad one'));
        await settle();
        expect(errors, hasLength(1));
        expect(c.turns.single.blocks.single.text, 'after the bad one');
      },
    );
  });

  group('attachments and disposal', () {
    test(
      'P-CFG-014: attachments need uploads config and are blocked while the own turn runs',
      () async {
        server.historyPages[null] = emptyPage();
        server.turnForMessages = 't9';
        await openWith(
          server,
          options: const HfyChatOptions(
            uploads: HfyUploadsConfig(types: ['image/png'], maxBytes: 10),
          ),
        );
        final c = kit.controller;
        await kit.connectLive();
        expect(c.attachDisabled, isFalse);
        c.setDraft('go');
        await c.submitDraft();
        expect(c.attachDisabled, isTrue, reason: 'own turn active');
      },
    );

    test(
      'P-CFG-014: no uploads config means attachments are impossible',
      () async {
        await openWith(server);
        expect(kit.controller.attachDisabled, isTrue);
      },
    );

    test(
      'dispose: an injected client is closed, and a pending notification is dropped quietly',
      () async {
        server.historyPages[null] = emptyPage();
        await openWith(server);
        final c = kit.controller;
        await kit.connectLive();
        c.setDraft('pending');
        c.dispose();
        await settle();
        expect(kit.kit.client.state.value, HfyConnectionState.ended);
      },
    );

    test('dispose inside a notification is safe', () async {
      await openWith(server);
      final c = kit.controller;
      c.addListener(() => c.dispose());
      var notified = false;
      c.addListener(() => notified = true);
      c.setDraft('x');
      await until(() => notified);
      expect(c.draft, 'x');
      // dispose() inside the notification defers the base dispose until the
      // notification ends, so the base class is disposed by now.
      expect(
        () => c.addListener(() {}),
        throwsFlutterError,
        reason: 'the deferred base dispose ran',
      );
    });

    test('open() owns and disposes its client', () async {
      final timers = FakeTimers()..now = 0;
      server.historyPages[null] = emptyPage();
      final c = HfyChatController.open(
        api: server.base,
        tokenProvider: (reason) async => 'tok',
        clientOptions: HfyClientOptions(
          clock: () => timers.now,
          timerFactory: timers.factory,
          idleTimerFactory: timers.factory,
        ),
      );
      await c.connect();
      expect(c.connection, HfyConnectionState.live);
      c.dispose();
      await settle();
      expect(c.connection, HfyConnectionState.ended);
    });
  });
}

/// The same envelope without its id (a history page may carry one).
Map<String, Object?> withoutId(Map<String, Object?> envelope) =>
    Map<String, Object?>.of(envelope)..remove('id');

/// One SSE frame's wire text (the same shape FakeSseConnection.addFrame writes).
String _frameText(Map<String, Object?> envelope) =>
    'id: ${envelope['id']}\nevent: ${envelope['type']}\ndata: ${jsonEncode(envelope)}\n\n';
