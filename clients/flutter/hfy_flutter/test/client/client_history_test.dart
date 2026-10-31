// History behavior: pagination semantics, viewer capture order and the
// cursor rules of PARITY.md P-CLIENT-006/009/015/018/019 (history deliveries
// never advance lastEventId; live_after is pinned by connect(); ONE page
// per loadOlder call).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  test('P-CLIENT-006/018: history page 1 fires in wire order; the viewer is '
      'captured BEFORE the first listener runs; live_after pins the cursor',
      () async {
    server.historyPages[null] = historyPage(
      turns: [
        {
          'id': 'turn-1',
          'events': [
            wireEnv('e1', 'turn'),
            wireEnv('e2', 'turn.state', parent: 'e1',
                data: {'state': 'running'}),
          ],
        },
        {
          'id': 'turn-2',
          'events': [wireEnv('e3', 'message.text', parent: 'e2')],
        },
      ],
      next: null,
      liveAfter: 'la-page1',
    );
    final ids = <String>[];
    kit.client.events.listen((event) {
      // The viewer MUST be readable already (client.js captures before
      // firing, client.js:188).
      ids.add(event.id!);
      expect(kit.client.viewer!.uuid, 'viewer-uuid');
    });
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.state, HfyConnectionState.live);
    expect(ids, ['e1', 'e2', 'e3']);
    expect(kit.client.viewer!.type, 'user');
    expect(kit.client.viewer!.ref, 'alice');
    expect(kit.client.lastEventId, 'la-page1',
        reason: 'live_after wins over any last fired / history id');
    expect(kit.client.hasOlder, isFalse);
  });

  test('P-CLIENT-009: loadOlder without a cursor returns false WITHOUT a '
      'request', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la');
    await kit.client.connect();
    await kit.waitLive();
    final before = server.requests.length;
    expect(await kit.client.loadOlder(), isFalse);
    expect(server.requests.length, before);
  });

  test('P-CLIENT-009/018/019: loadOlder fetches ONE page per call on the '
      'next cursor; its events fire as history; a next-less page ends it',
      () async {
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't1', 'events': [wireEnv('a', 'turn')]},
        ],
        liveAfter: 'la',
        next: 't1');
    server.historyPages['t1'] = historyPage(
        turns: [
          {'id': 't0', 'events': [wireEnv('b', 'turn')]},
        ],
        liveAfter: 'la2',
        next: null);
    await kit.client.connect();
    await kit.waitLive();
    expect(kit.client.hasOlder, isTrue);
    final events = <String>[];
    kit.client.events.listen((event) => events.add(event.id!));
    expect(await kit.client.loadOlder(), isFalse,
        reason: 'this page has no next: exactly one page fetched');
    expect(kit.client.hasOlder, isFalse);
    expect(events, ['b']);
    final loadOlderCall = server.requestWhere(
        (r) => Uri.parse(r.path).queryParameters['before'] == 't1');
    expect(loadOlderCall.authorization, 'Bearer tok-1');
    // The history delivery did NOT move the cursor (P-CLIENT-015).
    expect(kit.client.lastEventId, 'la');
  });

  test('P-CLIENT-009: hasOlder chains page by page, ONE page per call, until '
      'next is null', () async {
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't2', 'events': [wireEnv('a', 'turn')]},
        ],
        liveAfter: 'la',
        next: 't2');
    server.historyPages['t2'] = historyPage(
        turns: [
          {'id': 't1', 'events': [wireEnv('b', 'turn')]},
        ],
        liveAfter: 'la2',
        next: 't1');
    server.historyPages['t1'] = historyPage(
        turns: [
          {'id': 't0', 'events': [wireEnv('c', 'turn')]},
        ],
        liveAfter: 'la3',
        next: null);
    await kit.client.connect();
    await kit.waitLive();
    expect(await kit.client.loadOlder(), isTrue,
        reason: 'page t2 -> next t1');
    expect(await kit.client.loadOlder(), isFalse,
        reason: 'page t1 is the last (next null): ONE page per call');
    expect(await kit.client.loadOlder(), isFalse, reason: 'exhausted');
    expect(await kit.client.loadOlder(), isFalse,
        reason: 'a null cursor short-circuits without a request');
    final cursors = server.requests
        .where((r) => Uri.parse(r.path).queryParameters.containsKey('before'))
        .map((r) => Uri.parse(r.path).queryParameters['before'])
        .toList();
    expect(cursors, ['t2', 't1'], reason: 'one request per paging call');
  });

  test('P-CLIENT-018: the before cursor is encoded exactly like client.js '
      '(encodeURIComponent)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la', next: 'a b+c');
    server.historyPages['a b+c'] = historyPage(liveAfter: 'la2', next: null);
    await kit.client.connect();
    await kit.waitLive();
    await kit.client.loadOlder();
    final call = server.requestWhere(
        (r) => Uri.parse(r.path).queryParameters.containsKey('before'));
    expect(call.path.contains('before=a%20b%2Bc'), isTrue);
  });

  test('deliveringHistory is true only while history envelopes fire: the '
      'boot page and each loadOlder page yes, live SSE frames no', () async {
    server.historyPages[null] = historyPage(
        turns: [
          {
            'id': 't1',
            'events': [
              wireEnv('h1', 'turn'),
              wireEnv('h2', 'message.text', parent: 'h1'),
            ],
          },
        ],
        next: 'c1',
        liveAfter: 'la');
    server.historyPages['c1'] = historyPage(
        turns: [
          {'id': 't0', 'events': [wireEnv('h0', 'turn')]},
        ],
        liveAfter: 'la');
    final seen = <String>[];
    kit.client.events.listen((event) =>
        seen.add('${event.id}:${kit.client.deliveringHistory}'));
    await kit.client.connect();
    await kit.waitLive();
    await kit.client.loadOlder();
    await kit.server.sseConnections.last
        .addFrame(wireEnv('L1', 'message.text', parent: 'h1'));
    await until(() => seen.length == 4);
    expect(seen, ['h1:true', 'h2:true', 'h0:true', 'L1:false']);
    expect(kit.client.deliveringHistory, isFalse);
  });

  test('deliveringHistory resets when a boot page throws mid-delivery',
      () async {
    final flags = <bool>[];
    kit.client.events
        .listen((event) => flags.add(kit.client.deliveringHistory));
    // A non-object turn makes HfyHistoryPage.eventsOf throw a TypeError after
    // the first envelope has fired.
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't1', 'events': [wireEnv('h1', 'turn')]},
          7,
        ],
        liveAfter: 'la',
        next: 'c1');
    await expectLater(kit.client.connect(), throwsA(isA<TypeError>()));
    expect(flags, [true], reason: 'h1 fired while the page was delivered');
    expect(kit.client.deliveringHistory, isFalse,
        reason: 'the finally resets the flag');
  });

  test('deliveringHistory resets when a loadOlder page throws mid-delivery',
      () async {
    final flags = <bool>[];
    kit.client.events
        .listen((event) => flags.add(kit.client.deliveringHistory));
    server.historyPages[null] = historyPage(
        turns: [
          {'id': 't2', 'events': [wireEnv('a', 'turn')]},
        ],
        liveAfter: 'la',
        next: 't2');
    server.historyPages['t2'] = historyPage(
        turns: [
          {'id': 't1', 'events': [wireEnv('b', 'turn')]},
          7,
        ],
        liveAfter: 'la',
        next: null);
    await kit.client.connect();
    await kit.waitLive();
    await expectLater(kit.client.loadOlder(), throwsA(isA<TypeError>()));
    expect(flags, [true, true], reason: 'a and b fired as history');
    expect(kit.client.deliveringHistory, isFalse);
  });
}
