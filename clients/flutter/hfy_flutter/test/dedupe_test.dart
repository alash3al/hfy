// Dedupe tests — EXACTLY the rule of clients/fixtures/README.md ("Duplicate
// delivery") and HfyClient's #fire (client.js:165-173, PARITY.md
// P-CLIENT-015): a seen-set by envelope id, first delivery wins, history is
// seeded (dropped on replay), only LIVE deliveries advance lastEventId,
// live_after is pinned by connect(), and closed means nothing fires.
// Scenarios replicate the fixtures events/dedupe-duplicate.json and
// events/history-then-live.json (P-FOLD-015's silent types still advance
// the cursor by envelope id; type is irrelevant to dedupe).
// in-package test; HfyDedupe deliberately stays OFF the public barrel.
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/dedupe.dart';

import 'helpers/fixture_loader.dart';

void main() {
  group('P-CLIENT-015 dedupe-duplicate.json (fixtures README rule)', () {
    test('replayed delta ids are dropped entirely — text stays "Hello"', () {
      final fixture = fixtureFile('dedupe-duplicate');
      final envelopes = fixture['events']! as List<Object?>;
      final dedupe = HfyDedupe();
      // live path (plain #fire): only first-deliveries reach the fold.
      final fired = <String>[];
      for (final envelope in envelopes.cast<Map<String, dynamic>>()) {
        if (dedupe.claim(envelope['id'] as String?, isHistory: false)) {
          fired.add('${envelope['id']}');
        }
      }
      // ids in fixture order (dup ev-4/ev-7 dropped): turn-1, ev-2, ev-3,
      // ev-4, ev-5, turn-2, ev-7
      expect(fired, const <String>[
        'turn-1',
        'ev-2',
        'ev-3',
        'ev-4',
        'ev-5',
        'turn-2',
        'ev-7',
      ]);
      // the lost duplicate delta (text 'He' again) can never double-append;
      // the README's "would read HeHel… instead of Hello…" failsafe stands.
      expect(dedupe.lastEventId, 'ev-7',
          reason: 'cursor advanced to the last first-delivery (dup dropped before it)');
    });

    test('each id delivers exactly once across separate claim waves', () {
      final dedupe = HfyDedupe();
      expect(dedupe.claim('a', isHistory: false), isTrue);
      expect(dedupe.claim('a', isHistory: true), isFalse, reason: 'seen');
      expect(dedupe.claim('a', isHistory: false), isFalse, reason: 'seen (live replay)');
      expect(dedupe.lastEventId, 'a', reason: 'advanced on the first live delivery only');
    });

    test('an envelope WITHOUT an id always delivers, never moves the cursor', () {
      final dedupe = HfyDedupe();
      expect(dedupe.claim(null, isHistory: false), isTrue);
      expect(dedupe.claim(null, isHistory: false), isTrue);
      expect(dedupe.claim(null, isHistory: true), isTrue);
      expect(dedupe.lastEventId, isNull);
    });
  });

  group('P-CLIENT-015 history-then-live.json (history seeds; liveAfter pins)', () {
    test('history seeding drops a live replay; liveAfter pins the cursor', () {
      final fixture = fixtureFile('history-then-live');
      final events = fixture['events']! as List<Object?>;
      final goHistory = events.cast<Map<String, dynamic>>().where((e) => e['history'] == true).toList();
      final goLive = events.cast<Map<String, dynamic>>().where((e) => e['history'] != true).toList();

      final dedupe = HfyDedupe();
      // history page fires as-is (client.js #loadHistory → #fire(env, true));
      // nothing moves the cursor.
      final historyFired = <String>[];
      for (final envelope in goHistory) {
        if (dedupe.claim(envelope['id'] as String?, isHistory: true)) {
          historyFired.add(envelope['id']! as String);
        }
      }
      expect(historyFired, const <String>['turn-1', 'ev-2', 'ev-3']);
      expect(dedupe.lastEventId, isNull,
          reason: 'README rule 2: history deliveries never touch the cursor');

      // connect(): live_after wins — pinned AFTER the history run
      // (fixtures README: "After the leading history run the harness pins
      // liveAfter … as HfyClient#connect does").
      dedupe.pinLiveAfter(fixture['liveAfter']! as String);
      expect(dedupe.lastEventId, 'ev-3');

      // the live turn keeps flowing
      final liveFired = <String>[];
      for (final envelope in goLive.cast<Map<String, dynamic>>()) {
        if (dedupe.claim(envelope['id'] as String?, isHistory: false)) {
          liveFired.add(envelope['id']! as String);
        }
      }
      expect(liveFired, const <String>['turn-2', 'ev-5', 'ev-6', 'ev-7']);
      expect(dedupe.lastEventId, 'ev-7');

      // a reconnect replays an already-delivered LIVE delta under the same
      // envelope id — dropped, so text cannot double-append (README).
      expect(dedupe.claim('ev-7', isHistory: false), isFalse);
      expect(dedupe.lastEventId, 'ev-7');

      // …and the history replay of ev-3 (older page overlap) is dropped too
      expect(dedupe.claim('ev-3', isHistory: false), isFalse);
      expect(dedupe.claim('ev-3', isHistory: true), isFalse);
      expect(dedupe.lastEventId, 'ev-7');
    });
  });

  group('P-CLIENT-014/015 closed dedupe refuses everything', () {
    test('after close(), nothing delivers and the cursor freezes', () {
      final dedupe = HfyDedupe()..pinLiveAfter('start');
      dedupe.close();
      expect(dedupe.closed, isTrue);
      expect(dedupe.claim('new', isHistory: false), isFalse);
      expect(dedupe.claim(null, isHistory: false), isFalse);
      expect(dedupe.claim('new', isHistory: true), isFalse);
      expect(dedupe.lastEventId, 'start');
    });

    test('a normal seen-set keeps ids even with null-id envelopes around', () {
      final dedupe = HfyDedupe();
      expect(dedupe.claim(null, isHistory: false), isTrue);
      expect(dedupe.claim('x', isHistory: false), isTrue);
      expect(dedupe.claim(null, isHistory: false), isTrue, reason: 'null ids never dedupe');
      expect(dedupe.claim('x', isHistory: false), isFalse);
    });
  });
}
