// Event model tests: envelope round-trips, unknown types, malformed shapes —
// plus a sweep over EVERY envelope in clients/fixtures/events/*.json.
// PARITY IDs: P-CLIENT-001 (EVENT_TYPES), P-FOLD-001-adjacent data tolerance
// (ui.js hfyData), §1 envelope shape (docs/reference.md, client.js).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/hfy_flutter.dart';

import 'helpers/fixture_loader.dart';

void main() {
  group('P-CLIENT-001 EVENT_TYPES', () {
    test('the client listens to exactly the 14 named SSE types', () {
      expect(
        EVENT_TYPES,
        unorderedEquals(const <String>[
          'turn', 'turn.state', 'turn.cancel_requested', 'message.text',
          'reasoning', 'tool_call', 'tool_call.interrupted', 'tool_result',
          'approval.requested', 'approval.decided', 'model.usage', 'summary',
          'text.delta', 'reasoning.delta',
        ]),
      );
    });
  });

  group('fixture sweep — every envelope in clients/fixtures/events', () {
    test('file headers shape (task: each file has a header plus events)', () {
      for (final file in fixtureFiles()) {
        expect(file['name'], isA<String>(), reason: 'header name');
        expect(file['path'], anyOf('live', 'history'), reason: 'header path');
        expect(file['viewer'], isNotNull, reason: 'viewer of ${file['name']}');
        expect(file['events'], isA<List<Object?>>(), reason: 'events of ${file['name']}');
      }
    });

    test('every envelope (events + pages) parses and round-trips', () {
      for (final file in fixtureFiles()) {
        final name = file['name'] as String;
        final pages = (file['pages'] as List<Object?>?) ?? const <Object?>[];
        final rawEnvelopes = <Object?>[
          ...(file['events']! as List<Object?>),
          for (final page in pages) ...(page as Map)['events']! as List<Object?>,
        ];
        for (final raw in rawEnvelopes) {
          expect(raw, isA<Map<String, dynamic>>(), reason: 'envelopes are objects in $name');
          final source = raw! as Map<String, dynamic>;
          final event = HfyEvent.fromJson(raw);
          // Round-trip: id / type / parent / actor / v / data (deliverable).
          expect(event.id, source['id'], reason: 'id in $name');
          expect(event.type, source['type'], reason: 'type in $name');
          expect(event.parent, source['parent'], reason: 'parent in $name');
          expect(
            event.actor,
            source['actor'] == null
                ? isNull
                : actorOf(source['actor']! as Map<String, dynamic>),
            reason: 'actor in $name (${event.id})',
          );
          expect(event.v, source['v'], reason: 'v in $name (${event.id})');
          expect(event.data, isA<Map<String, dynamic>>(), reason: 'data shape');
          if (source['data'] is Map) {
            expect(event.data, Map<String, dynamic>.from(source['data']! as Map),
                reason: 'data in $name (${event.id})');
          }
          // created_at is real RFC3339 in fixtures (README rule) → parses to
          // the exact instant it was written with.
          final createdAtRaw = source['created_at'];
          if (createdAtRaw is String) {
            expect(event.createdAt, isNotNull,
                reason: 'created_at parses in $name (${event.id})');
            expect(
              event.createdAt!.millisecondsSinceEpoch,
              DateTime.parse(createdAtRaw).millisecondsSinceEpoch,
              reason: 'created_at round-tup in $name (${event.id})',
            );
          } else {
            expect(event.createdAt, isNull, reason: 'created_at in $name');
          }
          // Re-parsing the event's own json yields the same value object.
          expect(HfyEvent.fromJson(event.json), event,
              reason: 'self round-trip in $name');
        }
      }
    });

    test('unknown event types are kept, not dropped (P-CLIENT-001/015 spirit)', () {
      final event = HfyEvent.fromJson(const <String, Object?>{
        'id': 'e-unknown',
        'type': 'future.banner',
        'parent': 'turn-1',
        'actor': {'uuid': 'u1', 'type': 'agent', 'ref': 'main'},
        'v': 1,
        'data': {'x': 1},
        'created_at': '2026-01-02T10:00:00Z',
      });
      expect(event.type, 'future.banner');
      expect(event.isKnownType, isFalse);
      expect(HfyEvent(id: 'x', type: 'turn').isKnownType, isTrue);
    });
  });

  group('HfyEvent.fromJson', () {
    test('tolerates absent optional fields', () {
      final event = HfyEvent.fromJson(const <String, Object?>{'type': 'turn'});
      expect(event.id, isNull);
      expect(event.parent, isNull);
      expect(event.actor, isNull);
      expect(event.v, isNull);
      expect(event.data, isEmpty);
      expect(event.createdAt, isNull);
    });

    test('data as a JSON string parses (hfyData parity, ui.js:113-117)', () {
      final event = HfyEvent.fromJson(const <String, Object?>{
        'type': 'tool_call',
        'data': '{"call_id":"c1","arguments":{"a":1}}',
      });
      expect(
        event.data,
        const {'call_id': 'c1', 'arguments': {'a': 1}},
      );
    });

    test('data as an unparsable JSON string degrades to {} (hfyData)', () {
      final event =
          HfyEvent.fromJson(const <String, Object?>{'type': 'tool_result', 'data': '{oops'});
      expect(event.data, isEmpty);
    });

    test('data absent/null → {}', () {
      expect(HfyEvent.fromJson(const {'type': 'turn'}).data, isEmpty);
      expect(HfyEvent.fromJson(const {'type': 'turn', 'data': null}).data, isEmpty);
    });

    test('created_at tolerant to absence and to junk (fold falls back to now)', () {
      final junk =
          HfyEvent.fromJson(const {'type': 'turn', 'created_at': 'not-a-date'});
      final wrongType = HfyEvent.fromJson(const {'type': 'turn', 'created_at': 42});
      final absent = HfyEvent.fromJson(const {'type': 'turn'});
      expect(junk.createdAt, isNull);
      expect(wrongType.createdAt, isNull);
      expect(absent.createdAt, isNull);
    });

    test('v accepts an integral double, rejects a fraction (typed)', () {
      expect(HfyEvent.fromJson(const {'type': 'turn', 'v': 1.0}).v, 1);
      expect(
        () => HfyEvent.fromJson(const {'type': 'turn', 'v': 1.5}),
        throwsHfyProtocol(),
      );
    });

    test('json keeps the raw envelope map', () {
      const raw = <String, Object?>{
        'id': 'a', 'type': 'turn', 'parent': null, 'data': {'agent': 'main'},
      };
      final event = HfyEvent.fromJson(raw);
      expect(Map<Object, Object?>.from(event.json), raw);
    });

    // Malformed shapes: each one throws the TYPED exception, never an
    // untyped crash (deliverable).
    final malformed = <String, Object Function()>{
      'not an object': () => 'turn-1',
      'list envelope': () => const <Object?>['nope'],
      'missing type': () => const {'id': 'a'},
      'empty type': () => const {'type': ''},
      'type not a string': () => const {'type': 5},
      'id not a string': () => const {'type': 'turn', 'id': 5},
      'parent not a string': () => const {'type': 'turn', 'parent': <Object?>[]},
      'actor not an object': () => const {'type': 'turn', 'actor': 'u1'},
      'actor field not a string': () => const {
          'type': 'turn',
          'actor': {'uuid': 5},
        },
      'data of a bad shape': () => const {'type': 'turn', 'data': 7},
      'data array': () => const {'type': 'turn', 'data': <Object?>['x']},
    };
    for (final entry in malformed.entries) {
      test('malformed: ${entry.key} → HfyProtocolException', () {
        expect(() => HfyEvent.fromJson(entry.value()), throwsHfyProtocol());
      });
    }
  });

  group('equality', () {
    test('same content, different object/key order → equal', () {
      final a = HfyEvent.fromJson(const {
        'id': 'a',
        'type': 'tool_call',
        'data': {'arguments': {'b': 1, 'a': 2}},
      });
      final b = HfyEvent.fromJson(const {
        'id': 'a',
        'type': 'tool_call',
        'data': {'arguments': {'a': 2, 'b': 1}},
      });
      expect(a, b);
      expect(a.hashCode, b.hashCode);
    });

    test('different content → not equal', () {
      final a = HfyEvent.fromJson(const {
        'id': 'a',
        'type': 'tool_call',
        'data': {'arguments': {'b': 1}},
      });
      final other = HfyEvent.fromJson(const {
        'id': 'a',
        'type': 'tool_call',
        'data': {'z': 1},
      });
      expect(a == other, isFalse);
    });
  });
}

Matcher actorOf(Map<String, dynamic> source) => predicate<HfyActor?>(
      (actor) =>
          actor != null &&
          actor.uuid == source['uuid'] &&
          actor.type == source['type'] &&
          actor.ref == source['ref'],
      'actor {uuid, type, ref} of $source',
    );

Matcher throwsHfyProtocol() => throwsA(isA<HfyProtocolException>());
