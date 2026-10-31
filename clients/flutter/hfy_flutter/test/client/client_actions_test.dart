// The promise methods against the wire: byte-exact payload shapes
// (P-CLIENT-010/011/012/013), the full upload flow in both flavors
// (P-CLIENT-011 / uploads.go), config() (P-CFG-004 client side) and the error
// mapping table of P-CLIENT-022 / docs/reference.md. One testP covers the
// 401-retry re-sending the body (P-CLIENT-021).
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/client/errors.dart';

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit kit;
  setUp(() async {
    server = await FakeHfyServer.start();
    kit = await ClientKit.open(server);
  });
  tearDown(() => server.destroy());

  test('P-CLIENT-010: send() POSTs {text} BYTE-EXACT (no attachments key '
      'when undefined) and returns the turn id (202 {turn})', () async {
    kit.tokens.queueGrant('tok-send', expiresAtMs: 60000);
    final turn = await kit.client.send('hi');
    expect(turn, 'turn-fixed-1');
    expect(server.messagesBodies.single, '{"text":"hi"}');
    final req = server.requestWhereOrNull((r) => r.path.startsWith('/v1/conversation/messages'))!;
    expect(req.contentType, 'application/json');
  });

  test('P-CLIENT-010: attachments ride when DEFINED, in the {file, mime, '
      'name?} wire shape, key order exact', () async {
    final ref = HfyAttachmentRef(
        file: 'conversations/abc/x.png', mime: 'image/png', name: 'x.png');
    await kit.client.send('see', attachments: [ref.toWire()]);
    expect(server.messagesBodies.single,
        '{"text":"see","attachments":[{"file":"conversations/abc/x.png",'
        '"mime":"image/png","name":"x.png"}]}');
    // name-less refs drop the key too (client.js upload drops it).
    await kit.client.send('again', attachments: [
      const HfyAttachmentRef(file: 'f', mime: 'm').toWire(),
    ]);
    expect(server.messagesBodies.last,
        '{"text":"again","attachments":[{"file":"f","mime":"m"}]}');
  });

  test('P-CLIENT-012: approve() bodies are exact ({call_id, approve, '
      'result?, answers?} omitted when undefined)', () async {
    await kit.client.approve('c1', true);
    expect(server.approvalsBodies.single, '{"call_id":"c1","approve":true}');

    await kit.client.approve('c2', false, result: 'too risky');
    expect(server.approvalsBodies.last,
        '{"call_id":"c2","approve":false,"result":"too risky"}');

    await kit.client.approve('c3', true, answers: {'q1': 'a', 'q2': 1});
    expect(server.approvalsBodies.last,
        '{"call_id":"c3","approve":true,"answers":{"q1":"a","q2":1}}');
  });

  test('P-CLIENT-013: stop() POSTs with NO body and NO content-type',
      () async {
    await kit.client.stop();
    final req = server.requestWhereOrNull(
        (r) => r.path.startsWith('/v1/conversation/stop'))!;
    expect(req.method, 'POST');
    expect(req.body, '');
    expect(req.contentType, '');
  });

  test('P-CLIENT-011: the DIRECT (presigned) upload flow — raw PUT, no auth '
      'header, exact content-type, body bytes exact; ref = {file, mime?, '
      'name?}', () async {
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c/k.png',
      'upload_url': '${server.base}/v1/files/serve-direct',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': true,
    };
    final ref = await kit.client.upload(
      mime: 'image/png',
      size: 2,
      name: 'shot.png',
      bytes: Stream<List<int>>.value('AB'.codeUnits),
    );
    expect(ref.file, 'uploads/c/k.png');
    expect(ref.mime, 'image/png');
    expect(ref.name, 'shot.png');
    final put = server.uploadPuts.single;
    expect(put.path, '/v1/files/serve-direct');
    expect(put.authorization, '',
        reason: 'presigned: NO Authorization header');
    expect(put.contentType, 'image/png');
    expect(server.uploadPutBodies.single, 'AB');
    // The slot request itself was {mime, size} byte-exact.
    final slot = server.requestWhereOrNull(
        (r) => r.path.startsWith('/v1/conversation/uploads') &&
            r.method == 'POST')!;
    expect(slot.body, '{"mime":"image/png","size":2}');
  });

  test('P-CLIENT-011: the HFY-PATH upload flow — PUT to api + upload_url '
      'WITH the grant', () async {
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c/k.txt',
      'upload_url':
          '/v1/conversation/uploads/uploads%2Fc%2Fk.txt?exp=100&sig=S',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': false,
    };
    final ref = await kit.client.upload(
      mime: 'text/plain',
      size: 3,
      name: 'doc.txt',
      bytes: Stream<List<int>>.value('txt'.codeUnits),
    );
    expect(ref.name, 'doc.txt');
    final put = server.uploadPuts.single;
    expect(put.authorization, 'Bearer tok-1',
        reason: 'the hfy path carries the grant');
    expect(put.contentType, 'text/plain');
    expect(put.path.contains('/uploads%2Fc%2Fk.txt'), isTrue);
    expect(server.uploadPutBodies.single, 'txt');
  });

  test('P-CLIENT-011: a failing PUT is the typed "upload failed: <status>"',
      () async {
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c/k.png',
      'upload_url': '${server.base}/v1/files/serve-direct',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': true,
    };
    server.scriptedErrors.add(('/v1/files/serve-direct', 403, 'forbidden'));
    await expectLater(
        kit.client.upload(
            mime: 'image/png',
            size: 1,
            name: 'a.png',
            bytes: Stream<List<int>>.value('A'.codeUnits)),
        throwsA(predicate<HfyHttpException>((e) =>
            e.status == 403 && e.message == 'upload failed: 403')));
  });

  test('P-CFG-004 side: config() returns uploads null and the shaped form',
      () async {
    server.configUploads = null;
    expect((await kit.client.config()).uploads, isNull);
    server.configUploads = <String, Object?>{
      'types': ['image/png', 'image/jpeg'],
      'max_bytes': 140,
    };
    final cfg = await kit.client.config();
    expect(cfg.uploads!.types, ['image/png', 'image/jpeg']);
    expect(cfg.uploads!.maxBytes, 140);
    // A malformed uploads object degrades to null (P-CFG-004's rule).
    server.configUploads = <String, Object?>{'types': <String>[], 'max_bytes': 0};
    expect((await kit.client.config()).uploads, isNull);
  });

  test('P-CLIENT-022: every status maps to the typed message of '
      'docs/reference.md (401/403 are HfyAuthException, the rest plain)',
      () async {
    final cases = <(int, String)>[
      (400, 'text or attachments required'),
      (401, 'no token'),
      (403, 'not your turn'),
      (404, 'not found'),
      (409, 'no pending approval'),
      (413, 'body over 2 MiB'),
    ];
    for (final (status, error) in cases) {
      // A 401 always consumes TWO responses: the attempt and its exactly-one
      // retry (P-CLIENT-021); the second one fails with retries disabled, so
      // the typed 401 surfaces either way.
      if (status == 401) {
        server.scriptedErrors
            .add(('/v1/conversation/messages', status, error));
      }
      server.scriptedErrors
          .add(('/v1/conversation/messages', status, error));
      await expectLater(kit.client.send('hi'),
          throwsA(isA<HfyHttpException>()
              .having((e) => e.status, 'status', status)
              .having((e) => e.message, 'message', error)));
    }
    // The typed subtype is by-status exactly (P-CLIENT-021: the 401
    // consumes the attempt and its one retry, both scripted).
    server.scriptedErrors
        .add(('/v1/conversation/messages', 401, 'again'));
    server.scriptedErrors
        .add(('/v1/conversation/messages', 401, 'again'));
    Object? caught;
    try {
      await kit.client.send('hi');
    } on HfyHttpException catch (e) {
      caught = e;
    }
    expect(caught, isA<HfyAuthException>(), reason: '401 -> HfyAuthException');
    server.scriptedErrors
        .add(('/v1/conversation/messages', 400, 'again'));
    caught = null;
    try {
      await kit.client.send('hi');
    } on HfyHttpException catch (e) {
      caught = e;
    }
    expect(caught, isA<HfyHttpException>().having(
        (e) => e.runtimeType, 'not an auth subtype', HfyHttpException),
        reason: '400 stays a plain HfyHttpException');
  });

  test('P-CLIENT-021: the 401 retry re-sends the body once with the fresh '
      'token and then succeeds', () async {
    server.schedule401();
    final turn = await kit.client.send('hi again');
    expect(turn, 'turn-fixed-1');
    final messageRequests = server.requests
        .where((r) => r.path.startsWith('/v1/conversation/messages'))
        .toList();
    expect(messageRequests, hasLength(2),
        reason: 'one attempt (401) + one retry, both with the full body');
    expect(messageRequests[0].body, messageRequests[1].body,
        reason: 'the retry carries the same body byte-exact');
    expect(messageRequests[0].authorization, 'Bearer tok-1');
    expect(messageRequests[1].authorization, 'Bearer tok-2',
        reason: 'the retry used the fresh token');
    expect(server.messagesBodies, ['{"text":"hi again"}'],
        reason: 'only the retried attempt reached the 202 path');
  });

  test('P-CLIENT-014: a REST error surfaces WITHOUT disturburing the '
      'connection state (state stays live, no events lost)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await kit.client.connect();
    await kit.waitLive();
    await until(() => kit.state == HfyConnectionState.live);
    server.scriptedErrors.add(
        ('/v1/conversation/messages', 409, 'no pending approval'));
    await expectLater(kit.client.send('x'), throwsA(isA<HfyHttpException>()));
    final ids = <String>[];
    kit.client.events.listen((event) => ids.add(event.id!));
    await server.sseConnections.last
        .addFrame(wireEnv('err-1', 'message.text'));
    await until(() => ids.length == 1);
    expect(ids, ['err-1']);
    expect(kit.state, HfyConnectionState.live);
  });
}
